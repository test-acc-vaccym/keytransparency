// Copyright 2018 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package keyserver

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/google/keytransparency/core/directory"
	"github.com/google/keytransparency/core/fake"
	"github.com/google/trillian/testonly"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/google/keytransparency/core/api/v1/keytransparency_go_proto"
	tpb "github.com/google/trillian"
)

const mapID = int64(2)

type miniEnv struct {
	s              *testonly.MockServer
	srv            *Server
	stopFakeServer func()
	stopController func()
}

func newMiniEnv(ctx context.Context, t *testing.T) (*miniEnv, error) {
	fakeAdmin := fake.NewDirectoryStorage()
	if err := fakeAdmin.Write(ctx, &directory.Directory{
		DirectoryID: directoryID,
		MapID:       mapID,
		MinInterval: 1 * time.Second,
		MaxInterval: 5 * time.Second,
	}); err != nil {
		return nil, fmt.Errorf("admin.Write(): %v", err)
	}

	ctrl := gomock.NewController(t)
	s, stopFakeServer, err := testonly.NewMockServer(ctrl)
	if err != nil {
		return nil, fmt.Errorf("Error starting fake server: %v", err)
	}
	srv := &Server{
		directories: fakeAdmin,
		tlog:        s.LogClient,
		tmap:        s.MapClient,
		indexFunc: func(context.Context, *directory.Directory, string) ([32]byte, []byte, error) {
			return [32]byte{}, []byte(""), nil
		},
	}
	return &miniEnv{
		s:              s,
		srv:            srv,
		stopController: ctrl.Finish,
		stopFakeServer: stopFakeServer,
	}, nil
}

func (e *miniEnv) Close() {
	e.stopController()
	e.stopFakeServer()
}

func TestLatestRevision(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		desc     string
		treeSize int64
		wantErr  codes.Code
		wantRev  int64
	}{
		{desc: "not initialized", treeSize: 0, wantErr: codes.Internal},
		{desc: "log controls revision", treeSize: 2, wantErr: codes.OK, wantRev: 1},
	} {
		tc := tc // pin
		t.Run(tc.desc+" GetUser", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			defer cancel()
			e, err := newMiniEnv(ctx, t)
			if err != nil {
				t.Fatalf("newMiniEnv(): %v", err)
			}
			defer e.Close()
			e.s.Log.EXPECT().GetLatestSignedLogRoot(gomock.Any(), gomock.Any()).
				Return(&tpb.GetLatestSignedLogRootResponse{
					SignedLogRoot: &tpb.SignedLogRoot{TreeSize: tc.treeSize},
				}, err)
			if tc.wantErr == codes.OK {
				e.s.Map.EXPECT().GetLeavesByRevision(gomock.Any(),
					&tpb.GetMapLeavesByRevisionRequest{
						MapId:    mapID,
						Index:    [][]byte{make([]byte, 32)},
						Revision: tc.treeSize - 1,
					}).
					Return(&tpb.GetMapLeavesResponse{
						MapLeafInclusion: []*tpb.MapLeafInclusion{{
							Leaf: &tpb.MapLeaf{
								Index: make([]byte, 32),
							},
						}},
					}, nil)
				e.s.Log.EXPECT().GetInclusionProof(gomock.Any(), gomock.Any()).
					Return(&tpb.GetInclusionProofResponse{}, nil)
			}

			_, err = e.srv.GetUser(ctx, &pb.GetUserRequest{DirectoryId: directoryID})
			if got, want := status.Code(err), tc.wantErr; got != want {
				t.Errorf("GetUser(): %v, want %v", err, want)
			}
		})
		t.Run(tc.desc+" GetUserHistory", func(t *testing.T) {
			e, err := newMiniEnv(ctx, t)
			if err != nil {
				t.Fatalf("newMiniEnv(): %v", err)
			}
			defer e.Close()
			e.s.Log.EXPECT().GetLatestSignedLogRoot(gomock.Any(), gomock.Any()).
				Return(&tpb.GetLatestSignedLogRootResponse{
					SignedLogRoot: &tpb.SignedLogRoot{TreeSize: tc.treeSize},
				}, err).Times(2)
			for i := int64(0); i < tc.treeSize; i++ {
				e.s.Map.EXPECT().GetLeavesByRevision(gomock.Any(),
					&tpb.GetMapLeavesByRevisionRequest{
						MapId:    mapID,
						Index:    [][]byte{make([]byte, 32)},
						Revision: i,
					}).
					Return(&tpb.GetMapLeavesResponse{
						MapLeafInclusion: []*tpb.MapLeafInclusion{{
							Leaf: &tpb.MapLeaf{
								Index: make([]byte, 32),
							},
						}},
					}, nil).Times(2)
				e.s.Log.EXPECT().GetInclusionProof(gomock.Any(), gomock.Any()).
					Return(&tpb.GetInclusionProofResponse{}, nil).Times(2)
			}

			_, err = e.srv.ListEntryHistory(ctx, &pb.ListEntryHistoryRequest{DirectoryId: directoryID})
			if got, want := status.Code(err), tc.wantErr; got != want {
				t.Errorf("ListEntryHistory(): %v, want %v", err, tc.wantErr)
			}
			_, err = e.srv.ListUserRevisions(ctx, &pb.ListUserRevisionsRequest{
				DirectoryId: directoryID,
				EndRevision: tc.treeSize - 1,
			})
			if got, want := status.Code(err), tc.wantErr; got != want {
				t.Errorf("ListUserRevisions(): %v, want %v", err, tc.wantErr)
			}
		})
	}

}
