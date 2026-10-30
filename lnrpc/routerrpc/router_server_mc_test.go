package routerrpc

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// queryMissionControlMock is a controllable MissionControl mock for the
// QueryMissionControl RPC.
type queryMissionControlMock struct {
	MissionControl

	snapshot *routing.MissionControlSnapshot
	page     *routing.MissionControlHistoryPage
	err      error

	lastPageSize  uint32
	lastPageToken []byte
}

func (m *queryMissionControlMock) GetHistorySnapshot() *routing.MissionControlSnapshot {
	return m.snapshot
}

func (m *queryMissionControlMock) QueryHistoryPage(pageSize uint32,
	pageToken []byte) (*routing.MissionControlHistoryPage, error) {

	m.lastPageSize = pageSize
	m.lastPageToken = pageToken

	return m.page, m.err
}

func newQueryMcServer(mc *queryMissionControlMock) *Server {
	return &Server{
		cfg: &Config{
			RouterBackend: &RouterBackend{
				MissionControl: mc,
			},
		},
	}
}

// TestQueryMissionControlProtoRoundTrip verifies that the pagination fields
// survive a proto marshal/unmarshal cycle, guarding the generated descriptor.
func TestQueryMissionControlProtoRoundTrip(t *testing.T) {
	req := &QueryMissionControlRequest{
		PageSize:  42,
		PageToken: []byte("opaque-token"),
	}

	reqBytes, err := proto.Marshal(req)
	require.NoError(t, err)

	var decodedReq QueryMissionControlRequest
	require.NoError(t, proto.Unmarshal(reqBytes, &decodedReq))
	require.True(t, proto.Equal(req, &decodedReq))
	require.Equal(t, req.PageSize, decodedReq.PageSize)
	require.Equal(t, req.PageToken, decodedReq.PageToken)

	resp := &QueryMissionControlResponse{
		Pairs: []*PairHistory{
			{
				NodeFrom: []byte{1},
				NodeTo:   []byte{2},
			},
		},
		NextPageToken: []byte("next-opaque-token"),
	}

	respBytes, err := proto.Marshal(resp)
	require.NoError(t, err)

	var decodedResp QueryMissionControlResponse
	require.NoError(t, proto.Unmarshal(respBytes, &decodedResp))
	require.True(t, proto.Equal(resp, &decodedResp))
	require.Equal(t, resp.NextPageToken, decodedResp.NextPageToken)
	require.Len(t, decodedResp.Pairs, 1)

	// The fields must be visible through proto reflection as well.
	reqDesc := req.ProtoReflect().Descriptor()
	require.NotNil(t, reqDesc.Fields().ByName("page_size"))
	require.NotNil(t, reqDesc.Fields().ByName("page_token"))

	respDesc := resp.ProtoReflect().Descriptor()
	require.NotNil(t, respDesc.Fields().ByName("next_page_token"))
}

// TestQueryMissionControlLegacy verifies that a request without pagination
// parameters returns the full history through the legacy path.
func TestQueryMissionControlLegacy(t *testing.T) {
	from := route.Vertex{1}
	to := route.Vertex{2}

	mc := &queryMissionControlMock{
		snapshot: &routing.MissionControlSnapshot{
			Pairs: []routing.MissionControlPairSnapshot{
				{
					Pair: routing.NewDirectedNodePair(
						from, to,
					),
					TimedPairResult: routing.TimedPairResult{
						SuccessAmt: lnwire.MilliSatoshi(
							1234,
						),
					},
				},
			},
		},
	}
	server := newQueryMcServer(mc)

	resp, err := server.QueryMissionControl(
		t.Context(), &QueryMissionControlRequest{},
	)
	require.NoError(t, err)
	require.Len(t, resp.Pairs, 1)
	require.Equal(t, from[:], resp.Pairs[0].NodeFrom)
	require.Equal(t, to[:], resp.Pairs[0].NodeTo)
	require.Empty(t, resp.NextPageToken)

	// The paginated path must not have been taken.
	require.Zero(t, mc.lastPageSize)
	require.Nil(t, mc.lastPageToken)
}

// TestQueryMissionControlPagination verifies that the first page and
// continuation requests are forwarded to mission control with the expected
// parameters and that the opaque token is returned unchanged.
func TestQueryMissionControlPagination(t *testing.T) {
	token := []byte{1, 2, 3, 4}

	mc := &queryMissionControlMock{
		page: &routing.MissionControlHistoryPage{
			Pairs: []routing.MissionControlPairSnapshot{
				{
					Pair: routing.NewDirectedNodePair(
						route.Vertex{9}, route.Vertex{8},
					),
				},
			},
			NextPageToken: token,
		},
	}
	server := newQueryMcServer(mc)

	// The first request creates a snapshot.
	resp, err := server.QueryMissionControl(
		t.Context(), &QueryMissionControlRequest{PageSize: 100},
	)
	require.NoError(t, err)
	require.Len(t, resp.Pairs, 1)
	require.Equal(t, token, resp.NextPageToken)
	require.Equal(t, uint32(100), mc.lastPageSize)
	require.Empty(t, mc.lastPageToken)

	// The continuation only carries the token and reuses the page size.
	resp, err = server.QueryMissionControl(
		t.Context(), &QueryMissionControlRequest{PageToken: token},
	)
	require.NoError(t, err)
	require.Len(t, resp.Pairs, 1)
	require.Equal(t, token, resp.NextPageToken)
	require.Zero(t, mc.lastPageSize)
	require.Equal(t, token, mc.lastPageToken)

	// A page size may be supplied on continuation and is passed through.
	_, err = server.QueryMissionControl(
		t.Context(), &QueryMissionControlRequest{
			PageSize:  100,
			PageToken: token,
		},
	)
	require.NoError(t, err)
	require.Equal(t, uint32(100), mc.lastPageSize)
}

// TestQueryMissionControlErrors verifies that pagination errors are mapped to
// explicit gRPC status codes instead of silently starting another export.
func TestQueryMissionControlErrors(t *testing.T) {
	testCases := []struct {
		name     string
		err      error
		expected codes.Code
	}{
		{
			name:     "page size out of range",
			err:      routing.ErrPageSizeOutOfRange,
			expected: codes.InvalidArgument,
		},
		{
			name:     "page size mismatch",
			err:      routing.ErrPageSizeMismatch,
			expected: codes.InvalidArgument,
		},
		{
			name:     "invalid token",
			err:      routing.ErrInvalidPageToken,
			expected: codes.InvalidArgument,
		},
		{
			name:     "expired snapshot",
			err:      routing.ErrHistorySnapshotExpired,
			expected: codes.FailedPrecondition,
		},
		{
			name:     "unknown snapshot",
			err:      routing.ErrHistorySnapshotNotFound,
			expected: codes.FailedPrecondition,
		},
		{
			name:     "snapshot capacity exhausted",
			err:      routing.ErrHistorySnapshotLimitExceeded,
			expected: codes.ResourceExhausted,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mc := &queryMissionControlMock{err: tc.err}
			server := newQueryMcServer(mc)

			_, err := server.QueryMissionControl(
				t.Context(), &QueryMissionControlRequest{
					PageSize: 1,
				},
			)
			require.Error(t, err)

			statusErr, ok := status.FromError(err)
			require.True(t, ok)
			require.Equal(t, tc.expected, statusErr.Code())
			require.NotEmpty(t, statusErr.Message())
		})
	}
}
