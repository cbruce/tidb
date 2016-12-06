// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package tikv

import (
	"time"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/kvproto/pkg/coprocessor"
	"github.com/pingcap/kvproto/pkg/errorpb"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
)

// RegionRequestSender sends KV/Cop requests to tikv server.
// It handles network errors and some region errors internally when it is safe to retry.
type RegionRequestSender struct {
	bo          *Backoffer
	regionCache *RegionCache
	client      Client
}

// NewRegionRequestSender creates a new sender.
func NewRegionRequestSender(bo *Backoffer, regionCache *RegionCache, client Client) *RegionRequestSender {
	return &RegionRequestSender{
		bo:          bo,
		regionCache: regionCache,
		client:      client,
	}
}

// SendKVReq sends a KV request to tikv server.
func (s *RegionRequestSender) SendKVReq(req *kvrpcpb.Request, regionID RegionVerID, timeout time.Duration) (*kvrpcpb.Response, error) {
	for {
		select {
		case <-s.bo.ctx.Done():
			return nil, errors.Trace(s.bo.ctx.Err())
		default:
		}

		region := s.regionCache.GetRegionByVerID(regionID)
		if region == nil {
			// If the region is not found in cache, it must be out
			// of date and already be cleaned up. We can skip the
			// RPC by returning RegionError directly.
			return &kvrpcpb.Response{
				Type:        req.GetType(),
				RegionError: &errorpb.Error{StaleEpoch: &errorpb.StaleEpoch{}},
			}, nil
		}

		resp, retry, err := s.sendKVReqToRegion(region, req, timeout)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if retry {
			continue
		}

		if regionErr := resp.GetRegionError(); regionErr != nil {
			retry, err := s.onRegionError(region, regionErr)
			if err != nil {
				return nil, errors.Trace(err)
			}
			if retry {
				continue
			}
		}

		if resp.GetType() != req.GetType() {
			return nil, errors.Trace(errMismatch(resp, req))
		}
		return resp, nil
	}
}

// SendCopReq sends a coprocessor request to tikv server.
func (s *RegionRequestSender) SendCopReq(req *coprocessor.Request, regionID RegionVerID, timeout time.Duration) (*coprocessor.Response, error) {
	for {
		region := s.regionCache.GetRegionByVerID(regionID)
		if region == nil {
			// If the region is not found in cache, it must be out
			// of date and already be cleaned up. We can skip the
			// RPC by returning RegionError directly.
			return &coprocessor.Response{
				RegionError: &errorpb.Error{StaleEpoch: &errorpb.StaleEpoch{}},
			}, nil
		}

		resp, retry, err := s.sendCopReqToRegion(region, req, timeout)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if retry {
			continue
		}

		if regionErr := resp.GetRegionError(); regionErr != nil {
			retry, err := s.onRegionError(region, regionErr)
			if err != nil {
				return nil, errors.Trace(err)
			}
			if retry {
				continue
			}
		}
		return resp, nil
	}
}

func (s *RegionRequestSender) sendKVReqToRegion(region *Region, req *kvrpcpb.Request, timeout time.Duration) (resp *kvrpcpb.Response, retry bool, err error) {
	req.Context = region.GetContext()
	resp, err = s.client.SendKVReq(region.GetAddress(), req, timeout)
	if err != nil {
		if e := s.onSendFail(region.VerID(), region.GetContext(), err); e != nil {
			return nil, false, errors.Trace(e)
		}
		return nil, true, nil
	}
	return
}

func (s *RegionRequestSender) sendCopReqToRegion(region *Region, req *coprocessor.Request, timeout time.Duration) (resp *coprocessor.Response, retry bool, err error) {
	req.Context = region.GetContext()
	resp, err = s.client.SendCopReq(region.GetAddress(), req, timeout)
	if err != nil {
		if e := s.onSendFail(region.VerID(), region.GetContext(), err); e != nil {
			return nil, false, errors.Trace(err)
		}
		return nil, true, nil
	}
	return
}

func (s *RegionRequestSender) onSendFail(regionID RegionVerID, ctx *kvrpcpb.Context, err error) error {
	s.regionCache.NextPeer(regionID)
	err = s.bo.Backoff(boTiKVRPC, errors.Errorf("send tikv request error: %v, ctx: %s, try next peer later", err, ctx))
	return errors.Trace(err)
}

func (s *RegionRequestSender) onRegionError(region *Region, regionErr *errorpb.Error) (retry bool, err error) {
	reportRegionError(regionErr)

	if notLeader := regionErr.GetNotLeader(); notLeader != nil {
		// Retry if error is `NotLeader`.
		log.Warnf("tikv reports `NotLeader`: %s, ctx: %s, retry later", notLeader, region.GetContext())
		s.regionCache.UpdateLeader(region.VerID(), notLeader.GetLeader().GetId())
		if notLeader.GetLeader() == nil {
			err = s.bo.Backoff(boRegionMiss, errors.Errorf("not leader: %v, ctx: %s", notLeader, region.GetContext()))
			if err != nil {
				return false, errors.Trace(err)
			}
		}
	} else if staleEpoch := regionErr.GetStaleEpoch(); staleEpoch != nil {
		log.Warnf("tikv reports `StaleEpoch`, ctx: %s, retry later", region.GetContext())
		err = s.regionCache.OnRegionStale(region, staleEpoch.NewRegions)
		return false, errors.Trace(err)
	} else if regionErr.GetServerIsBusy() != nil {
		log.Warnf("tikv reports `ServerIsBusy`, ctx: %s, retry later", region.GetContext())
		err = s.bo.Backoff(boServerBusy, errors.Errorf("server is busy"))
		if err != nil {
			return false, errors.Trace(err)
		}
	} else {
		// For other errors, we only drop cache here.
		// Because caller may need to re-split the request.
		log.Warnf("tikv reports region error: %s, ctx: %s", regionErr, region.GetContext())
		s.regionCache.DropRegion(region.VerID())
	}
	return true, nil

}
