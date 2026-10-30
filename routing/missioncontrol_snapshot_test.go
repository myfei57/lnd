package routing

import (
	"bytes"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// snapshotTestVertex returns a route vertex with the given first bytes set.
func snapshotTestVertex(bs ...byte) route.Vertex {
	var v route.Vertex
	copy(v[:], bs)

	return v
}

// setTestPairResults replaces the mission control pair state with the given
// pairs. The success amount is derived from the pair index to make every
// pair distinguishable in tests. The map of expected results is returned.
func setTestPairResults(t *testing.T, ctx *mcTestContext,
	pairs []DirectedNodePair) map[DirectedNodePair]TimedPairResult {

	t.Helper()

	expected := make(
		map[DirectedNodePair]TimedPairResult, len(pairs),
	)

	ctx.mc.mu.Lock()
	defer ctx.mc.mu.Unlock()

	ctx.mc.state.lastPairResult = make(map[route.Vertex]NodeResults)

	for i, pair := range pairs {
		results, ok := ctx.mc.state.lastPairResult[pair.From]
		if !ok {
			results = make(NodeResults)
			ctx.mc.state.lastPairResult[pair.From] = results
		}

		result := TimedPairResult{
			SuccessAmt: lnwire.MilliSatoshi(i + 1),
			SuccessTime: mcTestTime.Add(
				time.Duration(i) * time.Second,
			),
		}

		results[pair.To] = result
		expected[pair] = result
	}

	return expected
}

// expectedPairOrder returns the expected lexicographic ordering of the given
// pairs, sorted by the raw from/to public key bytes.
func expectedPairOrder(pairs []DirectedNodePair) []DirectedNodePair {
	sorted := append([]DirectedNodePair(nil), pairs...)
	sortPairs(sorted)

	return sorted
}

// sortPairs sorts pairs the same way as the history snapshot store does.
func sortPairs(pairs []DirectedNodePair) {
	for i := 0; i < len(pairs); i++ {
		for j := i + 1; j < len(pairs); j++ {
			fromCmp := bytes.Compare(pairs[i].From[:], pairs[j].From[:])
			if fromCmp > 0 || (fromCmp == 0 &&
				bytes.Compare(pairs[i].To[:], pairs[j].To[:]) > 0) {

				pairs[i], pairs[j] = pairs[j], pairs[i]
			}
		}
	}
}

// walkPages pages through a complete export starting with the given page
// size and returns all pairs in the order in which they were returned.
func walkPages(t *testing.T, ctx *mcTestContext,
	pageSize uint32) []MissionControlPairSnapshot {

	t.Helper()

	var (
		all       []MissionControlPairSnapshot
		pageToken []byte
	)

	for {
		page, err := ctx.mc.QueryHistoryPage(pageSize, pageToken)
		require.NoError(t, err)
		require.LessOrEqual(t, len(page.Pairs), int(pageSize))

		all = append(all, page.Pairs...)

		if len(page.NextPageToken) == 0 {
			break
		}

		pageToken = page.NextPageToken
	}

	return all
}

// assertPairSet asserts that the returned pairs equal the expected ordered
// pairs without duplicates or missing entries, carrying their full history
// fields.
func assertPairSet(t *testing.T, got []MissionControlPairSnapshot,
	expectedOrder []DirectedNodePair,
	expectedResults map[DirectedNodePair]TimedPairResult) {

	t.Helper()

	require.Len(t, got, len(expectedOrder))

	seen := make(map[DirectedNodePair]struct{}, len(expectedOrder))

	for i, pairSnapshot := range got {
		expectedPair := expectedOrder[i]

		require.Equal(t, expectedPair, pairSnapshot.Pair,
			"pair at index %v is out of order", i)

		_, dup := seen[pairSnapshot.Pair]
		require.False(t, dup, "duplicate pair %v", pairSnapshot.Pair)
		seen[pairSnapshot.Pair] = struct{}{}

		// The full history fields must be returned for every pair.
		require.Equal(t, expectedResults[expectedPair],
			pairSnapshot.TimedPairResult)
	}
}

// TestHistorySnapshotPagination verifies that paging through a snapshot
// returns all pairs ordered by the raw from/to bytes, without duplicates or
// missing entries, and that the last page has no continuation token.
func TestHistorySnapshotPagination(t *testing.T) {
	ctx := createMcTestContext(t)

	pairs := []DirectedNodePair{
		NewDirectedNodePair(
			snapshotTestVertex(2), snapshotTestVertex(5),
		),
		NewDirectedNodePair(
			snapshotTestVertex(3), snapshotTestVertex(7),
		),
		NewDirectedNodePair(
			snapshotTestVertex(1), snapshotTestVertex(9),
		),
		NewDirectedNodePair(
			snapshotTestVertex(2), snapshotTestVertex(1),
		),
		NewDirectedNodePair(
			snapshotTestVertex(1), snapshotTestVertex(3),
		),
	}
	results := setTestPairResults(t, ctx, pairs)

	expected := expectedPairOrder(pairs)

	// Exercise several page sizes, including a page size larger than the
	// number of pairs.
	for _, pageSize := range []uint32{1, 2, 3, 1000} {
		all := walkPages(t, ctx, pageSize)
		assertPairSet(t, all, expected, results)
	}

	// An empty history returns an empty first page without a token.
	emptyCtx := createMcTestContext(t)
	page, err := emptyCtx.mc.QueryHistoryPage(10, nil)
	require.NoError(t, err)
	require.Empty(t, page.Pairs)
	require.Empty(t, page.NextPageToken)
}

// TestHistorySnapshotRetryDeterministic verifies that retrying the same token
// returns the same page and the same next token.
func TestHistorySnapshotRetryDeterministic(t *testing.T) {
	ctx := createMcTestContext(t)

	pairs := []DirectedNodePair{
		NewDirectedNodePair(
			snapshotTestVertex(1), snapshotTestVertex(1),
		),
		NewDirectedNodePair(
			snapshotTestVertex(2), snapshotTestVertex(2),
		),
		NewDirectedNodePair(
			snapshotTestVertex(3), snapshotTestVertex(3),
		),
	}
	results := setTestPairResults(t, ctx, pairs)

	page1, err := ctx.mc.QueryHistoryPage(1, nil)
	require.NoError(t, err)
	require.Len(t, page1.Pairs, 1)
	require.NotEmpty(t, page1.NextPageToken)

	// Fetch page two and remember its token.
	page2, err := ctx.mc.QueryHistoryPage(0, page1.NextPageToken)
	require.NoError(t, err)
	require.Len(t, page2.Pairs, 1)
	require.NotEmpty(t, page2.NextPageToken)
	page2Token := append([]byte(nil), page2.NextPageToken...)

	// Retrying the page two token, with or without a matching page size,
	// returns the same page and the same continuation token.
	for _, requestedSize := range []uint32{0, 1} {
		retried, err := ctx.mc.QueryHistoryPage(
			requestedSize, page1.NextPageToken,
		)
		require.NoError(t, err)
		require.Equal(t, page2.Pairs, retried.Pairs)
		require.Equal(t, page2Token, retried.NextPageToken)
	}

	// The last page is reached with an empty next token and is returned
	// identically on retry.
	page3, err := ctx.mc.QueryHistoryPage(0, page2Token)
	require.NoError(t, err)
	require.Len(t, page3.Pairs, 1)
	require.Empty(t, page3.NextPageToken)

	retried, err := ctx.mc.QueryHistoryPage(1, page2Token)
	require.NoError(t, err)
	require.Equal(t, page3.Pairs, retried.Pairs)
	require.Empty(t, retried.NextPageToken)

	// Walking to the end covers every pair exactly once.
	all := walkPages(t, ctx, 1)
	assertPairSet(t, all, expectedPairOrder(pairs), results)
}

// TestHistorySnapshotIsolation verifies that payment results, imports and a
// history reset that happen while a paginated export is in progress neither
// change the exported contents nor cause duplicates or missing entries.
func TestHistorySnapshotIsolation(t *testing.T) {
	ctx := createMcTestContext(t)

	pairs := []DirectedNodePair{
		NewDirectedNodePair(
			snapshotTestVertex(1), snapshotTestVertex(1),
		),
		NewDirectedNodePair(
			snapshotTestVertex(2), snapshotTestVertex(2),
		),
		NewDirectedNodePair(
			snapshotTestVertex(3), snapshotTestVertex(3),
		),
		NewDirectedNodePair(
			snapshotTestVertex(4), snapshotTestVertex(4),
		),
		NewDirectedNodePair(
			snapshotTestVertex(5), snapshotTestVertex(5),
		),
	}
	results := setTestPairResults(t, ctx, pairs)

	// Start an export with a page size of two.
	page1, err := ctx.mc.QueryHistoryPage(2, nil)
	require.NoError(t, err)
	require.Len(t, page1.Pairs, 2)
	require.NotEmpty(t, page1.NextPageToken)
	page1Token := append([]byte(nil), page1.NextPageToken...)

	// New payment results arrive while the export is in progress.
	setTestPairResults(t, ctx, append(pairs,
		NewDirectedNodePair(
			snapshotTestVertex(6), snapshotTestVertex(6),
		),
		NewDirectedNodePair(
			snapshotTestVertex(7), snapshotTestVertex(7),
		),
	))

	page2, err := ctx.mc.QueryHistoryPage(0, page1Token)
	require.NoError(t, err)
	require.Len(t, page2.Pairs, 2)
	require.NotEmpty(t, page2.NextPageToken)

	// An import must not change the snapshot either.
	imported := []DirectedNodePair{
		NewDirectedNodePair(
			snapshotTestVertex(8), snapshotTestVertex(8),
		),
	}
	importSnapshot := &MissionControlSnapshot{
		Pairs: []MissionControlPairSnapshot{
			{
				Pair: imported[0],
				TimedPairResult: TimedPairResult{
					SuccessTime: mcTestTime,
				},
			},
		},
	}
	require.NoError(t, ctx.mc.ImportHistory(importSnapshot, false))

	// Finally reset the history completely.
	require.NoError(t, ctx.mc.ResetHistory())
	require.Empty(t, ctx.mc.GetHistorySnapshot().Pairs)

	page3, err := ctx.mc.QueryHistoryPage(0, page2.NextPageToken)
	require.NoError(t, err)
	require.Len(t, page3.Pairs, 1)
	require.Empty(t, page3.NextPageToken)

	// All pages together still cover exactly the snapshot's five pairs.
	all := append(append(
		append(
			[]MissionControlPairSnapshot(nil),
			page1.Pairs...,
		), page2.Pairs...,
	), page3.Pairs...)
	assertPairSet(t, all, expectedPairOrder(pairs), results)

	// Retrying an earlier page token after the reset returns the same page
	// and token from the fixed snapshot. The first response's token points
	// at page two, so it returns page two.
	retried, err := ctx.mc.QueryHistoryPage(0, page1Token)
	require.NoError(t, err)
	require.Equal(t, page2.Pairs, retried.Pairs)
	require.Equal(t, page2.NextPageToken, retried.NextPageToken)
}

// TestHistorySnapshotReadsDoNotMutate verifies that paging through the
// history does not change mission control's learned state.
func TestHistorySnapshotReadsDoNotMutate(t *testing.T) {
	ctx := createMcTestContext(t)

	pairs := []DirectedNodePair{
		NewDirectedNodePair(
			snapshotTestVertex(1), snapshotTestVertex(1),
		),
		NewDirectedNodePair(
			snapshotTestVertex(2), snapshotTestVertex(2),
		),
	}
	setTestPairResults(t, ctx, pairs)

	before := ctx.mc.GetHistorySnapshot()

	require.Len(t, walkPages(t, ctx, 1), len(pairs))
	require.Len(t, walkPages(t, ctx, 2), len(pairs))

	after := ctx.mc.GetHistorySnapshot()
	require.Len(t, after.Pairs, len(before.Pairs))
}

// TestHistorySnapshotPageSizeValidation verifies the page size boundaries for
// the initial request.
func TestHistorySnapshotPageSizeValidation(t *testing.T) {
	ctx := createMcTestContext(t)
	setTestPairResults(t, ctx, []DirectedNodePair{
		NewDirectedNodePair(
			snapshotTestVertex(1), snapshotTestVertex(1),
		),
	})

	// Zero is rejected at the pagination boundary; the RPC layer keeps
	// using the legacy full snapshot for zero page size.
	_, err := ctx.mc.QueryHistoryPage(0, nil)
	require.ErrorIs(t, err, ErrPageSizeOutOfRange)

	// Values above the maximum are rejected.
	_, err = ctx.mc.QueryHistoryPage(MaxHistoryPageSize+1, nil)
	require.ErrorIs(t, err, ErrPageSizeOutOfRange)

	// The boundaries themselves are accepted.
	for _, size := range []uint32{
		MinHistoryPageSize, MaxHistoryPageSize,
	} {
		page, err := ctx.mc.QueryHistoryPage(size, nil)
		require.NoError(t, err)
		require.Len(t, page.Pairs, 1)
	}
}

// TestHistorySnapshotContinuationPageSize verifies that the page size of a
// continuation request may be omitted but, when set, must match the initial
// page size.
func TestHistorySnapshotContinuationPageSize(t *testing.T) {
	ctx := createMcTestContext(t)
	setTestPairResults(t, ctx, []DirectedNodePair{
		NewDirectedNodePair(
			snapshotTestVertex(1), snapshotTestVertex(1),
		),
		NewDirectedNodePair(
			snapshotTestVertex(2), snapshotTestVertex(2),
		),
		NewDirectedNodePair(
			snapshotTestVertex(3), snapshotTestVertex(3),
		),
	})

	first, err := ctx.mc.QueryHistoryPage(2, nil)
	require.NoError(t, err)
	require.NotEmpty(t, first.NextPageToken)

	// Omitting the page size is accepted and reuses the initial size.
	page, err := ctx.mc.QueryHistoryPage(0, first.NextPageToken)
	require.NoError(t, err)
	require.Len(t, page.Pairs, 1)

	// A matching page size is accepted.
	page, err = ctx.mc.QueryHistoryPage(2, first.NextPageToken)
	require.NoError(t, err)
	require.Len(t, page.Pairs, 1)

	// A deviating page size is rejected.
	_, err = ctx.mc.QueryHistoryPage(1, first.NextPageToken)
	require.ErrorIs(t, err, ErrPageSizeMismatch)

	// Out-of-range values cannot masquerade as the snapshot's page size.
	_, err = ctx.mc.QueryHistoryPage(1001, first.NextPageToken)
	require.ErrorIs(t, err, ErrPageSizeMismatch)
}

// TestHistorySnapshotInvalidTokens verifies that malformed, forged and
// truncated tokens are rejected explicitly.
func TestHistorySnapshotInvalidTokens(t *testing.T) {
	ctx := createMcTestContext(t)
	setTestPairResults(t, ctx, []DirectedNodePair{
		NewDirectedNodePair(
			snapshotTestVertex(1), snapshotTestVertex(1),
		),
		NewDirectedNodePair(
			snapshotTestVertex(2), snapshotTestVertex(2),
		),
	})

	first, err := ctx.mc.QueryHistoryPage(1, nil)
	require.NoError(t, err)
	require.NotEmpty(t, first.NextPageToken)

	// Truncated, over-long and otherwise malformed tokens are rejected.
	for _, token := range [][]byte{
		[]byte("not-a-token"),
		make([]byte, pageTokenLen-1),
		make([]byte, pageTokenLen+1),
	} {
		_, err := ctx.mc.QueryHistoryPage(0, token)
		require.ErrorIs(t, err, ErrInvalidPageToken,
			"token %x", token)
	}

	// A valid-length random token cannot pass authentication.
	randomToken := make([]byte, pageTokenLen)
	randomToken[0] = pageTokenVersion
	_, err = ctx.mc.QueryHistoryPage(0, randomToken)
	require.ErrorIs(t, err, ErrInvalidPageToken)

	// Flipping a single bit of a valid token invalidates its
	// authenticator.
	forged := append([]byte(nil), first.NextPageToken...)
	forged[len(forged)-1] ^= 0x01
	_, err = ctx.mc.QueryHistoryPage(0, forged)
	require.ErrorIs(t, err, ErrInvalidPageToken)

	// An unsupported token version is rejected.
	wrongVersion := append([]byte(nil), first.NextPageToken...)
	wrongVersion[0] = pageTokenVersion + 1
	_, err = ctx.mc.QueryHistoryPage(0, wrongVersion)
	require.ErrorIs(t, err, ErrInvalidPageToken)
}

// TestHistorySnapshotForeignStore verifies that a token issued by another
// mission control instance (e.g. another node) is rejected instead of
// starting a new export.
func TestHistorySnapshotForeignStore(t *testing.T) {
	ctx := createMcTestContext(t)
	setTestPairResults(t, ctx, []DirectedNodePair{
		NewDirectedNodePair(
			snapshotTestVertex(1), snapshotTestVertex(1),
		),
		NewDirectedNodePair(
			snapshotTestVertex(2), snapshotTestVertex(2),
		),
	})

	first, err := ctx.mc.QueryHistoryPage(1, nil)
	require.NoError(t, err)
	require.NotEmpty(t, first.NextPageToken)

	// A different namespace is backed by a separate mission control
	// instance with its own token key.
	otherMC, err := ctx.mcController.GetNamespacedStore("other")
	require.NoError(t, err)

	_, err = otherMC.QueryHistoryPage(0, first.NextPageToken)
	require.ErrorIs(t, err, ErrInvalidPageToken)
}

// TestHistorySnapshotRestartInvalidates verifies that tokens issued before a
// restart are rejected by the restarted mission control instance.
func TestHistorySnapshotRestartInvalidates(t *testing.T) {
	ctx := createMcTestContext(t)
	setTestPairResults(t, ctx, []DirectedNodePair{
		NewDirectedNodePair(
			snapshotTestVertex(1), snapshotTestVertex(1),
		),
		NewDirectedNodePair(
			snapshotTestVertex(2), snapshotTestVertex(2),
		),
	})

	first, err := ctx.mc.QueryHistoryPage(1, nil)
	require.NoError(t, err)
	require.NotEmpty(t, first.NextPageToken)

	ctx.restartMc()

	_, err = ctx.mc.QueryHistoryPage(0, first.NextPageToken)
	require.ErrorIs(t, err, ErrInvalidPageToken)
}

// TestHistorySnapshotExpiry verifies that snapshots and their tokens expire
// exactly after HistorySnapshotTTL.
func TestHistorySnapshotExpiry(t *testing.T) {
	ctx := createMcTestContext(t)
	setTestPairResults(t, ctx, []DirectedNodePair{
		NewDirectedNodePair(
			snapshotTestVertex(1), snapshotTestVertex(1),
		),
		NewDirectedNodePair(
			snapshotTestVertex(2), snapshotTestVertex(2),
		),
	})

	start := ctx.clock.Now()
	first, err := ctx.mc.QueryHistoryPage(1, nil)
	require.NoError(t, err)
	require.NotEmpty(t, first.NextPageToken)

	// Immediately before the TTL the token is still valid.
	ctx.clock.setTime(start.Add(HistorySnapshotTTL - time.Nanosecond))
	page, err := ctx.mc.QueryHistoryPage(0, first.NextPageToken)
	require.NoError(t, err)
	require.Len(t, page.Pairs, 1)

	// At and after the TTL the token is rejected as expired.
	ctx.clock.setTime(start.Add(HistorySnapshotTTL))
	_, err = ctx.mc.QueryHistoryPage(0, first.NextPageToken)
	require.ErrorIs(t, err, ErrHistorySnapshotExpired)

	ctx.clock.setTime(start.Add(HistorySnapshotTTL + time.Minute))
	_, err = ctx.mc.QueryHistoryPage(0, first.NextPageToken)
	require.ErrorIs(t, err, ErrHistorySnapshotExpired)
}

// TestHistorySnapshotCapacityLimit verifies that at most 64 snapshots are
// retained concurrently, that expired snapshots free capacity and that
// continuation requests are not blocked by a full capacity.
func TestHistorySnapshotCapacityLimit(t *testing.T) {
	ctx := createMcTestContext(t)
	setTestPairResults(t, ctx, []DirectedNodePair{
		NewDirectedNodePair(
			snapshotTestVertex(1), snapshotTestVertex(1),
		),
		NewDirectedNodePair(
			snapshotTestVertex(2), snapshotTestVertex(2),
		),
	})

	// Fill the snapshot capacity.
	var continuationToken []byte
	for i := 0; i < maxHistorySnapshots; i++ {
		page, err := ctx.mc.QueryHistoryPage(1, nil)
		require.NoError(t, err)
		if i == 0 {
			continuationToken = page.NextPageToken
		}
	}

	// The next snapshot creation is rejected.
	_, err := ctx.mc.QueryHistoryPage(1, nil)
	require.ErrorIs(t, err, ErrHistorySnapshotLimitExceeded)

	// Continuation requests against an existing snapshot still succeed.
	page, err := ctx.mc.QueryHistoryPage(0, continuationToken)
	require.NoError(t, err)
	require.Len(t, page.Pairs, 1)
	require.Empty(t, page.NextPageToken)

	// Once the retained snapshots expire, capacity is freed again.
	ctx.clock.setTime(ctx.clock.Now().Add(HistorySnapshotTTL))
	page, err = ctx.mc.QueryHistoryPage(1, nil)
	require.NoError(t, err)
	require.Len(t, page.Pairs, 1)
}
