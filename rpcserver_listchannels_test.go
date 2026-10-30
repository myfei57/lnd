package lnd

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// chanFilterAttrs describes the attributes of an open channel that the
// ListChannels filters operate on.
type chanFilterAttrs struct {
	name                         string
	isActive, isPrivate, isLocal bool
}

// filter applies the ListChannels filters the same way the RPC handler does:
// the optional peer filter first (a nil wanted peer matches every channel),
// then the remaining filters via matchListChannelsFilters. The returned
// slice preserves the input order.
func filterChannels(req *lnrpc.ListChannelsRequest, peer []byte,
	channels ...chanFilterAttrs) []string {

	result := make([]string, 0, len(channels))
	for _, c := range channels {
		// Stand in for the peer's identity: its name bytes stand in
		// for the compressed pubkey.
		nodePub := []byte(c.name)
		if len(peer) > 0 && string(nodePub) != string(peer) {
			continue
		}

		if !matchListChannelsFilters(
			req, c.isActive, c.isPrivate, c.isLocal,
		) {
			continue
		}

		result = append(result, c.name)
	}

	return result
}

// TestValidateListChannelsInitiator makes sure that only the documented
// initiator filter values are accepted, so unknown enum numbers and the
// unsupported INITIATOR_BOTH value result in an explicit parameter error.
func TestValidateListChannelsInitiator(t *testing.T) {
	testCases := []struct {
		name      string
		initiator lnrpc.Initiator
		expectErr bool
	}{
		{
			name:      "unknown is default and accepted",
			initiator: lnrpc.Initiator_INITIATOR_UNKNOWN,
		},
		{
			name:      "local accepted",
			initiator: lnrpc.Initiator_INITIATOR_LOCAL,
		},
		{
			name:      "remote accepted",
			initiator: lnrpc.Initiator_INITIATOR_REMOTE,
		},
		{
			name:      "both is not a valid open channel filter",
			initiator: lnrpc.Initiator_INITIATOR_BOTH,
			expectErr: true,
		},
		{
			name:      "unknown enum number 4 is rejected",
			initiator: lnrpc.Initiator(4),
			expectErr: true,
		},
		{
			name:      "large unknown enum number is rejected",
			initiator: lnrpc.Initiator(127),
			expectErr: true,
		},
		{
			name:      "negative enum number is rejected",
			initiator: lnrpc.Initiator(-1),
			expectErr: true,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := validateListChannelsInitiator(tc.initiator)
			if tc.expectErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), "initiator")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestMatchListChannelsFiltersInitiator exercises the initiator filter on a
// mix of locally and remotely initiated channels, both on its own and
// intersected with the existing active/inactive/public/private filters. It
// also asserts that omitting the parameter preserves results and order, and
// that impossible combinations yield an empty result.
func TestMatchListChannelsFiltersInitiator(t *testing.T) {
	// A mix of channels: 3 initiated by this node, 3 by the remote peer,
	// with varying online and privacy states.
	channels := []chanFilterAttrs{
		{name: "local-active-public", isActive: true, isLocal: true},
		{name: "local-active-private", isActive: true, isPrivate: true,
			isLocal: true},
		{name: "local-inactive-public", isLocal: true},
		{name: "remote-active-public", isActive: true},
		{name: "remote-inactive-public"},
		{name: "remote-active-private", isActive: true,
			isPrivate: true},
	}

	testCases := []struct {
		name     string
		req      *lnrpc.ListChannelsRequest
		peer     []byte
		set      []chanFilterAttrs
		expected []string
	}{
		{
			name:     "no filter returns every channel in order",
			req:      &lnrpc.ListChannelsRequest{},
			set:      channels,
			expected: []string{"local-active-public", "local-active-private", "local-inactive-public", "remote-active-public", "remote-inactive-public", "remote-active-private"},
		},
		{
			name: "explicit unknown behaves like no filter",
			req: &lnrpc.ListChannelsRequest{
				Initiator: lnrpc.Initiator_INITIATOR_UNKNOWN,
			},
			set:      channels,
			expected: []string{"local-active-public", "local-active-private", "local-inactive-public", "remote-active-public", "remote-inactive-public", "remote-active-private"},
		},
		{
			name: "local initiator only",
			req: &lnrpc.ListChannelsRequest{
				Initiator: lnrpc.Initiator_INITIATOR_LOCAL,
			},
			set: channels,
			expected: []string{
				"local-active-public", "local-active-private",
				"local-inactive-public",
			},
		},
		{
			name: "remote initiator only",
			req: &lnrpc.ListChannelsRequest{
				Initiator: lnrpc.Initiator_INITIATOR_REMOTE,
			},
			set: channels,
			expected: []string{
				"remote-active-public",
				"remote-inactive-public",
				"remote-active-private",
			},
		},
		{
			name: "local intersect active only",
			req: &lnrpc.ListChannelsRequest{
				ActiveOnly: true,
				Initiator:  lnrpc.Initiator_INITIATOR_LOCAL,
			},
			set: channels,
			expected: []string{
				"local-active-public", "local-active-private",
			},
		},
		{
			name: "local intersect inactive only",
			req: &lnrpc.ListChannelsRequest{
				InactiveOnly: true,
				Initiator:    lnrpc.Initiator_INITIATOR_LOCAL,
			},
			set:      channels,
			expected: []string{"local-inactive-public"},
		},
		{
			name: "local intersect public only",
			req: &lnrpc.ListChannelsRequest{
				PublicOnly: true,
				Initiator:  lnrpc.Initiator_INITIATOR_LOCAL,
			},
			set: channels,
			expected: []string{
				"local-active-public", "local-inactive-public",
			},
		},
		{
			name: "local intersect private only",
			req: &lnrpc.ListChannelsRequest{
				PrivateOnly: true,
				Initiator:   lnrpc.Initiator_INITIATOR_LOCAL,
			},
			set:      channels,
			expected: []string{"local-active-private"},
		},
		{
			name: "remote intersect active only",
			req: &lnrpc.ListChannelsRequest{
				ActiveOnly: true,
				Initiator:  lnrpc.Initiator_INITIATOR_REMOTE,
			},
			set: channels,
			expected: []string{
				"remote-active-public", "remote-active-private",
			},
		},
		{
			name: "remote intersect inactive and public",
			req: &lnrpc.ListChannelsRequest{
				InactiveOnly: true,
				PublicOnly:   true,
				Initiator:    lnrpc.Initiator_INITIATOR_REMOTE,
			},
			set:      channels,
			expected: []string{"remote-inactive-public"},
		},
		{
			name: "remote intersect private and active",
			req: &lnrpc.ListChannelsRequest{
				ActiveOnly:  true,
				PrivateOnly: true,
				Initiator:   lnrpc.Initiator_INITIATOR_REMOTE,
			},
			set:      channels,
			expected: []string{"remote-active-private"},
		},
		{
			name: "local intersect peer filter",
			req: &lnrpc.ListChannelsRequest{
				Initiator: lnrpc.Initiator_INITIATOR_LOCAL,
			},
			peer:     []byte("local-active-private"),
			set:      channels,
			expected: []string{"local-active-private"},
		},
		{
			name: "remote peer excluded by local initiator filter",
			req: &lnrpc.ListChannelsRequest{
				Initiator: lnrpc.Initiator_INITIATOR_LOCAL,
			},
			peer:     []byte("remote-active-public"),
			set:      channels,
			expected: []string{},
		},
		{
			name: "local filter yields empty on all remote set",
			req: &lnrpc.ListChannelsRequest{
				Initiator: lnrpc.Initiator_INITIATOR_LOCAL,
			},
			set:      []chanFilterAttrs{channels[3], channels[4], channels[5]},
			expected: []string{},
		},
		{
			name: "impossible intersection yields empty",
			req: &lnrpc.ListChannelsRequest{
				InactiveOnly: true,
				PrivateOnly:  true,
				Initiator:    lnrpc.Initiator_INITIATOR_REMOTE,
			},
			set:      channels,
			expected: []string{},
		},
		{
			name: "existing active only filter is unaffected",
			req: &lnrpc.ListChannelsRequest{
				ActiveOnly: true,
			},
			set: channels,
			expected: []string{
				"local-active-public", "local-active-private",
				"remote-active-public", "remote-active-private",
			},
		},
		{
			name: "existing private only filter is unaffected",
			req: &lnrpc.ListChannelsRequest{
				PrivateOnly: true,
			},
			set: channels,
			expected: []string{
				"local-active-private", "remote-active-private",
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := filterChannels(tc.req, tc.peer, tc.set...)
			require.Equal(t, tc.expected, got)
		})
	}
}

// TestListChannelsInitiatorFilterOnEmptySet ensures that every filter returns
// an empty result when there are no open channels.
func TestListChannelsInitiatorFilterOnEmptySet(t *testing.T) {
	for _, req := range []*lnrpc.ListChannelsRequest{
		{},
		{Initiator: lnrpc.Initiator_INITIATOR_UNKNOWN},
		{Initiator: lnrpc.Initiator_INITIATOR_LOCAL},
		{Initiator: lnrpc.Initiator_INITIATOR_REMOTE},
		{ActiveOnly: true, Initiator: lnrpc.Initiator_INITIATOR_LOCAL},
	} {
		require.Empty(t, filterChannels(req, nil))
	}
}

// TestListChannelsInitiatorWireRoundTrip verifies the behavior that matters
// over the gRPC boundary: omitting the parameter does not touch the wire,
// valid values round trip, and unknown enum numbers remain observable to the
// server-side validation.
func TestListChannelsInitiatorWireRoundTrip(t *testing.T) {
	t.Run("omitted parameter is zero value and not serialized",
		func(t *testing.T) {
			req := &lnrpc.ListChannelsRequest{}
			b, err := proto.Marshal(req)
			require.NoError(t, err)
			require.Empty(t, b)

			dec := &lnrpc.ListChannelsRequest{}
			require.NoError(t, proto.Unmarshal(b, dec))
			require.Equal(t,
				lnrpc.Initiator_INITIATOR_UNKNOWN,
				dec.Initiator)
			require.NoError(t,
				validateListChannelsInitiator(dec.Initiator))
		})

	for _, v := range []lnrpc.Initiator{
		lnrpc.Initiator_INITIATOR_UNKNOWN,
		lnrpc.Initiator_INITIATOR_LOCAL,
		lnrpc.Initiator_INITIATOR_REMOTE,
		lnrpc.Initiator_INITIATOR_BOTH,
		lnrpc.Initiator(99),
	} {
		v := v
		t.Run(v.String(), func(t *testing.T) {
			req := &lnrpc.ListChannelsRequest{Initiator: v}
			b, err := proto.Marshal(req)
			require.NoError(t, err)

			dec := &lnrpc.ListChannelsRequest{}
			require.NoError(t, proto.Unmarshal(b, dec))
			require.Equal(t, v, dec.Initiator)

			err = validateListChannelsInitiator(dec.Initiator)
			switch v {
			case lnrpc.Initiator_INITIATOR_UNKNOWN,
				lnrpc.Initiator_INITIATOR_LOCAL,
				lnrpc.Initiator_INITIATOR_REMOTE:
				require.NoError(t, err)
			default:
				require.Error(t, err)
			}
		})
	}
}
