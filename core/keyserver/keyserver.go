// Copyright 2016 Google Inc. All Rights Reserved.
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

// Package keyserver implements a transparent key server for End to End.
package keyserver

import (
	"context"

	"github.com/google/keytransparency/core/crypto/vrf/p256"
	"github.com/google/keytransparency/core/directory"
	"github.com/google/keytransparency/core/mutator"

	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/empty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/google/keytransparency/core/api/v1/keytransparency_go_proto"
	rtpb "github.com/google/keytransparency/core/keyserver/readtoken_go_proto"
	spb "github.com/google/keytransparency/core/sequencer/sequencer_go_proto"
	tpb "github.com/google/trillian"
)

// MutationLogs provides sets of time ordered message logs.
type MutationLogs interface {
	// Send submits an item to a random log.
	Send(ctx context.Context, directoryID string, mutation ...*pb.EntryUpdate) error
	// ReadLog returns the messages in the (low, high] range stored in the specified log.
	ReadLog(ctx context.Context, directoryID string, logID, low, high int64,
		batchSize int32) ([]*mutator.LogMessage, error)
}

// BatchReader reads batch definitions.
type BatchReader interface {
	// ReadBatch returns the batch definitions for a given revision.
	ReadBatch(ctx context.Context, directoryID string, rev int64) (*spb.MapMetadata, error)
}

// indexFunc computes an index and proof for directory/user
type indexFunc func(ctx context.Context, d *directory.Directory, userID string) ([32]byte, []byte, error)

// Server holds internal state for the key server.
type Server struct {
	tlog        tpb.TrillianLogClient
	tmap        tpb.TrillianMapClient
	logAdmin    tpb.TrillianAdminClient
	mapAdmin    tpb.TrillianAdminClient
	mutate      mutator.ReduceMutationFn
	directories directory.Storage
	logs        MutationLogs
	batches     BatchReader
	indexFunc   indexFunc
}

// New creates a new instance of the key server.
func New(tlog tpb.TrillianLogClient,
	tmap tpb.TrillianMapClient,
	logAdmin tpb.TrillianAdminClient,
	mapAdmin tpb.TrillianAdminClient,
	mutate mutator.ReduceMutationFn,
	directories directory.Storage,
	logs MutationLogs,
	batches BatchReader) *Server {
	return &Server{
		tlog:        tlog,
		tmap:        tmap,
		logAdmin:    logAdmin,
		mapAdmin:    mapAdmin,
		mutate:      mutate,
		directories: directories,
		logs:        logs,
		batches:     batches,
		indexFunc:   indexFromVRF,
	}
}

// GetUser returns a user's profile and proof that there is only one object for
// this user and that it is the same one being provided to everyone else.
// GetUser also supports querying past values by setting the revision field.
func (s *Server) GetUser(ctx context.Context, in *pb.GetUserRequest) (*pb.GetUserResponse, error) {
	resp, err := s.BatchGetUser(ctx, &pb.BatchGetUserRequest{
		DirectoryId:          in.DirectoryId,
		UserIds:              []string{in.UserId},
		LastVerifiedTreeSize: in.LastVerifiedTreeSize,
	})
	if err != nil {
		return nil, err
	}
	if len(resp.MapLeavesByUserId) != 1 {
		return nil, status.Errorf(codes.Internal, "wrong number of map leaves: %v, want 1", len(resp.MapLeavesByUserId))
	}
	leaf, ok := resp.MapLeavesByUserId[in.UserId]
	if !ok {
		return nil, status.Errorf(codes.Internal, "wrong leaf returned")
	}
	return &pb.GetUserResponse{
		Revision: resp.Revision,
		Leaf:     leaf,
	}, nil
}

// getUserByRevision returns an entry and its proofs.
// getUserByRevision does NOT populate the following fields:
// - LogRoot
// - LogConsistency
func (s *Server) getUserByRevision(ctx context.Context, sth *tpb.SignedLogRoot, d *directory.Directory, userID string,
	rev int64) (*pb.GetUserResponse, error) {
	resp, err := s.batchGetUserByRevision(ctx, sth, d, []string{userID}, rev)
	if err != nil {
		return nil, err
	}
	if len(resp.MapLeavesByUserId) != 1 {
		return nil, status.Errorf(codes.Internal, "wrong number of map leaves: %v, want 1", len(resp.MapLeavesByUserId))
	}
	leaf, ok := resp.MapLeavesByUserId[userID]
	if !ok {
		return nil, status.Errorf(codes.Internal, "wrong leaf returned")
	}
	return &pb.GetUserResponse{
		Revision: resp.Revision,
		Leaf:     leaf,
	}, nil
}

// batchGetUserByRevision returns entries and proofs for a list of users.
func (s *Server) batchGetUserByRevision(ctx context.Context, sth *tpb.SignedLogRoot, d *directory.Directory,
	userIDs []string, mapRevision int64) (*pb.BatchGetUserResponse, error) {
	if mapRevision < 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"Revision is %v, want >= 0", mapRevision)
	}

	indexes := make([][]byte, 0, len(userIDs))
	proofsByUser, usersByIndex, err := s.batchGetUserIndex(ctx, d, userIDs)
	if err != nil {
		return nil, err
	}
	for index := range usersByIndex {
		indexes = append(indexes, []byte(index))
	}

	getResp, err := s.tmap.GetLeavesByRevision(ctx, &tpb.GetMapLeavesByRevisionRequest{
		MapId:    d.MapID,
		Index:    indexes,
		Revision: mapRevision,
	})
	if err != nil {
		glog.Errorf("GetLeavesByRevision(%v, rev: %v): %v", d.MapID, mapRevision, err)
		return nil, status.Errorf(codes.Internal, "Failed fetching map leaf")
	}
	if got, want := len(getResp.MapLeafInclusion), len(userIDs); got != want {
		glog.Errorf("GetLeavesByRevision() len: %v, want %v", got, want)
		return nil, status.Errorf(codes.Internal, "Failed fetching map leaf")
	}
	leaves := make(map[string]*pb.MapLeaf)
	for _, mapLeafInclusion := range getResp.MapLeafInclusion {
		if mapLeafInclusion.Leaf == nil {
			return nil, status.Errorf(codes.Internal, "leaf is nil")
		}
		var committed *pb.Committed
		if mapLeafInclusion.Leaf.LeafValue != nil {
			extraData := mapLeafInclusion.Leaf.ExtraData
			if extraData == nil {
				return nil, status.Errorf(codes.Internal, "Missing commitment data")
			}
			committed = &pb.Committed{}
			if err := proto.Unmarshal(extraData, committed); err != nil {
				return nil, status.Errorf(codes.Internal, "Cannot read committed value")
			}
		}
		user, ok := usersByIndex[string(mapLeafInclusion.Leaf.GetIndex())]
		if !ok {
			return nil, status.Errorf(codes.Internal, "Returned index %x that was not requested",
				mapLeafInclusion.Leaf.GetIndex())
		}
		proof, ok := proofsByUser[user]
		if !ok {
			return nil, status.Errorf(codes.Internal, "Returned index %x that was not requested",
				mapLeafInclusion.Leaf.GetIndex())
		}

		mapIncl := mapLeafInclusion
		mapIncl.Leaf.Index = nil     // Remove index from the returned data to force clients verify the VRFProof.
		mapIncl.Leaf.ExtraData = nil // Remove extra data as it is a duplicate of Committed.
		leaves[user] = &pb.MapLeaf{
			VrfProof:     proof,
			Committed:    committed,
			MapInclusion: mapIncl,
		}
	}

	// SignedMapHead to SignedLogRoot inclusion proof.
	logInclusion, err := s.tlog.GetInclusionProof(ctx,
		&tpb.GetInclusionProofRequest{
			LogId: d.LogID,
			// SignedMapRoot must be placed in the log at MapRevision.
			// MapRevisions start at 0. Log leaves start at 0.
			LeafIndex: mapRevision,
			TreeSize:  sth.TreeSize, // nolint TODO(gbelvin): Verify sth first.
		})
	if err != nil {
		glog.Errorf("tlog.GetInclusionProof(%v): %v", d.LogID, err)
		return nil, status.Errorf(codes.Internal, "Cannot fetch log inclusion proof")
	}

	return &pb.BatchGetUserResponse{
		MapLeavesByUserId: leaves,
		Revision: &pb.Revision{
			MapRoot: &pb.MapRoot{
				MapRoot:      getResp.GetMapRoot(),
				LogInclusion: logInclusion.GetProof().GetHashes(),
			},
		},
	}, nil
}

// BatchGetUser returns a batch of users at the same revision.
func (s *Server) BatchGetUser(ctx context.Context, in *pb.BatchGetUserRequest) (*pb.BatchGetUserResponse, error) {
	if in.DirectoryId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Please specify a directory_id")
	}

	// Lookup log and map info.
	d, err := s.directories.Read(ctx, in.DirectoryId, false)
	if err != nil {
		glog.Errorf("adminstorage.Read(%v): %v", in.DirectoryId, err)
		return nil, status.Errorf(codes.Internal, "Cannot fetch directory info")
	}

	// Fetch latest revision.
	sth, consistencyProof, err := s.latestLogRootProof(ctx, d, in.GetLastVerifiedTreeSize())
	if err != nil {
		return nil, err
	}
	revision, err := mapRevisionFor(sth)
	if err != nil {
		glog.Errorf("latestRevision(log: %v, sth: %v): %v", d.LogID, sth, err)
		return nil, err
	}

	entryProofs, err := s.batchGetUserByRevision(ctx, sth, d, in.UserIds, revision)
	if err != nil {
		return nil, err
	}
	resp := &pb.BatchGetUserResponse{
		Revision: &pb.Revision{
			LatestLogRoot: &pb.LogRoot{
				LogRoot:        sth,
				LogConsistency: consistencyProof.GetHashes(),
			},
		},
	}
	proto.Merge(resp, entryProofs)
	return resp, nil
}

// BatchGetUserIndex returns indexes for users, computed with a verifiable random function.
func (s *Server) BatchGetUserIndex(ctx context.Context,
	in *pb.BatchGetUserIndexRequest) (*pb.BatchGetUserIndexResponse, error) {
	if in.DirectoryId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Please specify a directory_id")
	}
	d, err := s.directories.Read(ctx, in.DirectoryId, false)
	if err != nil {
		glog.Errorf("adminstorage.Read(%v): %v", in.DirectoryId, err)
		return nil, status.Errorf(codes.Internal, "Cannot fetch directory info")
	}
	proofsByUser, _, err := s.batchGetUserIndex(ctx, d, in.UserIds)
	if err != nil {
		return nil, err
	}
	return &pb.BatchGetUserIndexResponse{Proofs: proofsByUser}, nil
}

func (s *Server) batchGetUserIndex(ctx context.Context, d *directory.Directory,
	userIDs []string) (proofsByUser map[string][]byte, usersByIndex map[string]string, err error) {
	proofsByUser = make(map[string][]byte)
	usersByIndex = make(map[string]string)
	for _, userID := range userIDs {
		index, proof, err := s.indexFunc(ctx, d, userID)
		if err != nil {
			return nil, nil, err
		}
		proofsByUser[userID] = proof
		usersByIndex[string(index[:])] = userID
	}
	return proofsByUser, usersByIndex, nil
}

// ListEntryHistory returns a list of EntryProofs covering a period of time.
func (s *Server) ListEntryHistory(ctx context.Context, in *pb.ListEntryHistoryRequest) (*pb.ListEntryHistoryResponse, error) {
	// Lookup log and map info.
	directoryID := in.GetDirectoryId()
	if directoryID == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Please specify a directory_id")
	}
	d, err := s.directories.Read(ctx, directoryID, false)
	if err != nil {
		glog.Errorf("adminstorage.Read(%v): %v", directoryID, err)
		return nil, status.Errorf(codes.Internal, "Cannot fetch directory info")
	}

	// Fetch latest revision.
	sth, consistencyProof, err := s.latestLogRootProof(ctx, d, in.GetLastVerifiedTreeSize())
	if err != nil {
		return nil, err
	}
	currentRevision, err := mapRevisionFor(sth)
	if err != nil {
		glog.Errorf("latestRevision(log %v, sth%v): %v", d.LogID, sth, err)
		return nil, err
	}

	if err := validateListEntryHistoryRequest(in, currentRevision); err != nil {
		glog.Errorf("validateListEntryHistoryRequest(%v, %v): %v", in, currentRevision, err)
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request")
	}

	// TODO(gbelvin): fetch all history from trillian at once.
	// Get all GetUserResponse for all revisions in the range [start, start + in.PageSize].
	responses := make([]*pb.GetUserResponse, in.PageSize)
	for i := range responses {
		resp, err := s.getUserByRevision(ctx, sth, d, in.UserId, in.Start+int64(i))
		if err != nil {
			glog.Errorf("getUser failed for revision %v: %v", in.Start+int64(i), err)
			return nil, status.Errorf(codes.Internal, "GetUser failed")
		}
		proto.Merge(resp, &pb.GetUserResponse{
			Revision: &pb.Revision{
				// TODO(gbelvin): This is redundant and wasteful. Refactor response API.
				LatestLogRoot: &pb.LogRoot{
					LogRoot:        sth,
					LogConsistency: consistencyProof.GetHashes(),
				},
			},
		})
		responses[i] = resp
	}

	nextStart := in.Start + int64(len(responses))
	if nextStart > currentRevision {
		nextStart = 0
	}

	return &pb.ListEntryHistoryResponse{
		Values:    responses,
		NextStart: nextStart,
	}, nil
}

// ListUserRevisions returns a list of revisions covering a period of time.
func (s *Server) ListUserRevisions(ctx context.Context, in *pb.ListUserRevisionsRequest) (
	*pb.ListUserRevisionsResponse, error) {
	pageStart := in.StartRevision
	lastVerified := in.LastVerifiedTreeSize
	if in.PageToken != "" {
		token := &rtpb.ListUserRevisionsToken{}
		if err := DecodeToken(in.PageToken, token); err != nil {
			glog.Errorf("invalid page token %v: %v", in.PageToken, err)
			return nil, status.Errorf(codes.InvalidArgument, "Invalid page_token provided")
		}
		// last_verified_tree_size and page_token are allowed to change between paginated requests.
		// Clear them here both for comparison and for encoding next_page_token in the response.
		in.LastVerifiedTreeSize = 0
		in.PageToken = ""
		if !proto.Equal(in, token.Request) {
			return nil, status.Errorf(codes.InvalidArgument, "Request fields changed during pagination")
		}
		pageStart += token.RevisionsReturned
	}

	// Lookup log and map info.
	directoryID := in.DirectoryId
	if directoryID == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Please specify a directory_id")
	}
	d, err := s.directories.Read(ctx, directoryID, false)
	if err != nil {
		glog.Errorf("adminstorage.Read(%v): %v", directoryID, err)
		return nil, status.Errorf(codes.Internal, "Cannot fetch directory info")
	}

	// Fetch latest log root & consistency proof.
	sth, consistencyProof, err := s.latestLogRootProof(ctx, d, lastVerified)
	if err != nil {
		return nil, err
	}
	newestRevision, err := mapRevisionFor(sth)
	if err != nil {
		glog.Errorf("latestRevision(log %v, sth %v): %v", d.LogID, sth, err)
		return nil, err
	}

	numRevisions, err := validateListUserRevisionsRequest(in, pageStart, newestRevision)
	if err != nil {
		glog.Errorf("validateListUserRevisionsRequest(%v, %v, %v): %v", in, pageStart, newestRevision, err)
		return nil, status.Errorf(codes.InvalidArgument, "Invalid request")
	}

	// TODO(gbelvin): fetch all history from trillian at once.
	// Get all revisions in the range [start + offset, start + offset + numRevisions].
	revisions := make([]*pb.MapRevision, numRevisions)
	for i := range revisions {
		rev := pageStart + int64(i)
		resp, err := s.getUserByRevision(ctx, sth, d, in.UserId, rev)
		if err != nil {
			glog.Errorf("getUser failed for revision %v: %v", rev, err)
			return nil, status.Errorf(codes.Internal, "GetUser failed")
		}
		revisions[i] = &pb.MapRevision{
			MapRoot: resp.GetRevision().GetMapRoot(),
			MapLeaf: resp.GetLeaf(),
		}
	}

	// Add a page token to the response if more revisions can be fetched.
	token := ""
	if pageStart+numRevisions < in.EndRevision {
		tokenProto := &rtpb.ListUserRevisionsToken{
			Request:           in,
			RevisionsReturned: (pageStart - in.StartRevision) + numRevisions,
		}
		token, err = EncodeToken(tokenProto)
		if err != nil {
			glog.Errorf("error encoding page token: %v", err)
			return nil, status.Errorf(codes.Internal, "Error encoding pagination token")
		}
	}
	resp := &pb.ListUserRevisionsResponse{
		LatestLogRoot: &pb.LogRoot{
			LogRoot:        sth,
			LogConsistency: consistencyProof.GetHashes(),
		},
		MapRevisions:  revisions,
		NextPageToken: token,
	}
	return resp, nil
}

// BatchListUserRevisions returns a list of revisions covering a period of time.
func (s *Server) BatchListUserRevisions(ctx context.Context, in *pb.BatchListUserRevisionsRequest) (
	*pb.BatchListUserRevisionsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}

// QueueEntryUpdate updates a user's profile. If the user does not exist, a new profile will be created.
func (s *Server) QueueEntryUpdate(ctx context.Context, in *pb.UpdateEntryRequest) (*empty.Empty, error) {
	return s.BatchQueueUserUpdate(ctx, &pb.BatchQueueUserUpdateRequest{
		DirectoryId: in.DirectoryId,
		Updates:     []*pb.EntryUpdate{in.EntryUpdate},
	})
}

// BatchQueueUserUpdate updates a user's profile. If the user does not exist, a new profile will be created.
func (s *Server) BatchQueueUserUpdate(ctx context.Context, in *pb.BatchQueueUserUpdateRequest) (*empty.Empty, error) {
	if in.DirectoryId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Please specify a directory_id")
	}
	// Lookup log and map info.
	directory, err := s.directories.Read(ctx, in.DirectoryId, false)
	if err != nil {
		glog.Errorf("adminstorage.Read(%v): %v", in.DirectoryId, err)
		return nil, status.Errorf(codes.Internal, "Cannot fetch directory info")
	}
	vrfPriv, err := p256.NewFromWrappedKey(ctx, directory.VRFPriv)
	if err != nil {
		return nil, err
	}

	// Verify:
	// - Index to Key equality in SignedKV.
	// - Correct profile commitment.
	// - Correct key formats.
	for _, u := range in.Updates {
		if err := validateEntryUpdate(u, vrfPriv); err != nil {
			glog.Warningf("Invalid UpdateEntryRequest: %v", err)
			return nil, status.Errorf(codes.InvalidArgument, "Invalid request")
		}
	}

	// TODO(gbelvin): Should we validate mutations here? It is expensive in terms of latency.

	// Save mutation to the database.
	if err := s.logs.Send(ctx, directory.DirectoryID, in.Updates...); err != nil {
		glog.Errorf("mutations.Write failed: %v", err)
		return nil, status.Errorf(codes.Internal, "Mutation write error")
	}
	return &empty.Empty{}, nil
}

// GetDirectory returns all info tied to the specified directory.
//
// This API to get all necessary data needed to verify a particular
// key-server. Data contains for instance the tree-info, like for instance the
// log/map-id and the corresponding public-keys.
func (s *Server) GetDirectory(ctx context.Context, in *pb.GetDirectoryRequest) (*pb.Directory, error) {
	// Lookup log and map info.
	if in.DirectoryId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Please specify a directory_id")
	}
	directory, err := s.directories.Read(ctx, in.DirectoryId, false)
	if status.Code(err) == codes.NotFound {
		glog.Errorf("adminstorage.Read(%v): %v", in.DirectoryId, err)
		return nil, status.Errorf(codes.NotFound, "Directory %v not found", in.DirectoryId)
	} else if err != nil {
		glog.Errorf("adminstorage.Read(%v): %v", in.DirectoryId, err)
		return nil, status.Errorf(codes.Internal, "Cannot fetch directory info for %v", in.DirectoryId)
	}

	logTree, err := s.logAdmin.GetTree(ctx, &tpb.GetTreeRequest{TreeId: directory.LogID})
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"Cannot fetch log info for %v: %v", in.DirectoryId, err)
	}
	mapTree, err := s.mapAdmin.GetTree(ctx, &tpb.GetTreeRequest{TreeId: directory.MapID})
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"Cannot fetch map info for %v: %v", in.DirectoryId, err)
	}

	return &pb.Directory{
		DirectoryId: directory.DirectoryID,
		Log:         logTree,
		Map:         mapTree,
		Vrf:         directory.VRF,
		MinInterval: ptypes.DurationProto(directory.MinInterval),
		MaxInterval: ptypes.DurationProto(directory.MaxInterval),
	}, nil
}

// index returns the index and proof for directory/user
func indexFromVRF(ctx context.Context, d *directory.Directory, userID string) ([32]byte, []byte, error) {
	vrfPriv, err := p256.NewFromWrappedKey(ctx, d.VRFPriv)
	if err != nil {
		return [32]byte{}, nil, err
	}
	index, proof := vrfPriv.Evaluate([]byte(userID))
	return index, proof, nil
}
