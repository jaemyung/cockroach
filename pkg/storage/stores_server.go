// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Tristan Rice (rice@fn.lc)

package storage

import (
	"bytes"

	"github.com/pkg/errors"
	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/util/log"
)

// StoresServer handles store-addressed RPCs.
type StoresServer interface {
	FreezeServer
	ReservationServer
}

var _ StoresServer = Server{}

// Server implements StoresServer.
type Server struct {
	descriptor *roachpb.NodeDescriptor
	stores     *Stores
}

// MakeServer returns a new instance of Server.
func MakeServer(descriptor *roachpb.NodeDescriptor, stores *Stores) Server {
	return Server{descriptor, stores}
}

func (is Server) execStoreCommand(h StoreRequestHeader, f func(*Store) error) error {
	if h.NodeID != is.descriptor.NodeID {
		return errors.Errorf("request for NodeID %d cannot be served by NodeID %d",
			h.NodeID, is.descriptor.NodeID)
	}
	store, err := is.stores.GetStore(h.StoreID)
	if err != nil {
		return err
	}
	return f(store)
}

// PollFrozen implements the StoresServer interface.
func (is Server) PollFrozen(
	ctx context.Context, args *PollFrozenRequest,
) (*PollFrozenResponse, error) {
	resp := &PollFrozenResponse{}
	err := is.execStoreCommand(args.StoreRequestHeader,
		func(s *Store) error {
			resp.Results = s.FrozenStatus(args.CollectFrozen)
			return nil
		})
	return resp, err
}

// Reserve implements the StoresServer interface.
func (is Server) Reserve(
	ctx context.Context, req *ReservationRequest,
) (*ReservationResponse, error) {
	resp := &ReservationResponse{}
	err := is.execStoreCommand(req.StoreRequestHeader,
		func(s *Store) error {
			*resp = s.Reserve(ctx, *req)
			return nil
		})
	return resp, err
}

// Server implements ConsistencyServer.
var _ ConsistencyServer = Server{}

// CollectChecksum implements ConsistencyServer.
func (is Server) CollectChecksum(
	ctx context.Context, req *CollectChecksumRequest,
) (*CollectChecksumResponse, error) {
	resp := &CollectChecksumResponse{}
	err := is.execStoreCommand(req.StoreRequestHeader,
		func(s *Store) error {
			r, err := s.GetReplica(req.RangeID)
			if err != nil {
				return err
			}
			c, err := r.getChecksum(ctx, req.ChecksumID)
			if err != nil {
				return err
			}
			resp.Checksum = c.checksum
			if !bytes.Equal(req.Checksum, c.checksum) {
				log.Errorf(ctx, "consistency check failed on range ID %s: expected checksum %x, got %x",
					req.RangeID, req.Checksum, c.checksum)
				resp.Snapshot = c.snapshot
			}
			return nil
		})
	return resp, err
}
