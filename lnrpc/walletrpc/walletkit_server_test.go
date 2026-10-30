//go:build walletrpc
// +build walletrpc

package walletrpc

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
	"github.com/stretchr/testify/require"
)

// TestMarshallLeasesIncludesSpendProgress verifies that ListLeases exposes
// each confirmation-controlled lease's persisted spend progress.
func TestMarshallLeasesIncludesSpendProgress(t *testing.T) {
	t.Parallel()

	testCases := []int32{0, 123, -1}
	for _, spendHeight := range testCases {
		testName := fmt.Sprintf("height %d", spendHeight)
		t.Run(testName, func(t *testing.T) {
			t.Parallel()

			lockedOutput := &wtxmgr.LockedOutput{
				Outpoint:               wire.OutPoint{Index: 2},
				LockID:                 wtxmgr.LockID{1},
				Expiration:             time.Unix(123, 0),
				ReleaseAfterSpendConfs: 6,
				ConfirmedSpendHeight:   spendHeight,
			}
			locks := []*wallet.ListLeasedOutputResult{{
				LockedOutput: lockedOutput,
				Value:        1000,
				PkScript:     []byte{0x51},
			}}

			rpcLocks := marshallLeases(locks)
			require.Len(t, rpcLocks, 1)
			require.Equal(
				t, spendHeight,
				rpcLocks[0].ConfirmedSpendHeight,
			)
		})
	}
}

// TestWitnessTypeMapping tests that the two witness type enums in the `input`
// package and the `walletrpc` package remain equal.
func TestWitnessTypeMapping(t *testing.T) {
	t.Parallel()

	// Tests that both enum types have the same length except the
	// UNKNOWN_WITNESS type which is only present in the walletrpc
	// witness type enum.
	require.Equal(
		t, len(allWitnessTypes), len(WitnessType_name)-1,
		"number of witness types should match proto definition",
	)

	// Tests that the string representations of both enum types are
	// equivalent.
	for witnessType, witnessTypeProto := range allWitnessTypes {
		// Redeclare to avoid loop variables being captured
		// by func literal.

		t.Run(witnessType.String(), func(tt *testing.T) {
			tt.Parallel()

			witnessTypeName := witnessType.String()
			witnessTypeName = strings.ToUpper(witnessTypeName)
			witnessTypeProtoName := witnessTypeProto.String()
			witnessTypeProtoName = strings.ReplaceAll(
				witnessTypeProtoName, "_", "",
			)

			require.Equal(
				t, witnessTypeName, witnessTypeProtoName,
				"mapped witness types should be named the same",
			)
		})
	}
}

type mockCoinSelectionLocker struct {
	fail bool
}

// renewalWallet returns a persisted lease after extending it through the
// legacy lease method.
type renewalWallet struct {
	*leaseOptionsWallet

	leases  []*wallet.ListLeasedOutputResult
	listErr error
}

// ListLeasedOutputs returns the persisted leases visible after renewal.
func (w *renewalWallet) ListLeasedOutputs() (
	[]*wallet.ListLeasedOutputResult, error) {

	return w.leases, w.listErr
}

// WithCoinSelectLock runs the callback and optionally returns a test error.
func (m *mockCoinSelectionLocker) WithCoinSelectLock(f func() error) error {
	if err := f(); err != nil {
		return err
	}

	if m.fail {
		return fmt.Errorf("kek")
	}

	return nil
}

// TestLeaseOutputRejectsUnsupportedOptions verifies WalletKit does not
// silently downgrade an option-bearing request to a time-only lease.
func TestLeaseOutputRejectsUnsupportedOptions(t *testing.T) {
	t.Parallel()

	wallet := &legacyLeaseWallet{
		WalletController: &mock.WalletController{},
	}
	rpcServer, _, err := New(&Config{
		Wallet: &lnwallet.LightningWallet{
			WalletController: wallet,
		},
		CoinSelectionLocker: &mockCoinSelectionLocker{},
	})
	require.NoError(t, err)

	_, err = rpcServer.LeaseOutput(t.Context(), &LeaseOutputRequest{
		Id: bytes.Repeat([]byte{1}, 32),
		Outpoint: &lnrpc.OutPoint{
			TxidBytes:   make([]byte, 32),
			OutputIndex: 1,
		},
		ExpirationSeconds:      60,
		ReleaseAfterSpendConfs: 6,
	})
	require.ErrorIs(t, err, errOutputLeaseOptionsUnsupported)
	require.Zero(t, wallet.leaseCalls,
		"unsupported options must not create a shorter legacy lease")
}

// TestLeaseOutputReturnsEffectiveRenewalDepth verifies that renewing an
// existing confirmation-controlled lease through the legacy zero-depth path
// reports the non-zero depth retained by the wallet.
func TestLeaseOutputReturnsEffectiveRenewalDepth(t *testing.T) {
	t.Parallel()

	lockID := wtxmgr.LockID{1}
	outpoint := wire.OutPoint{Index: 1}
	controller := &renewalWallet{
		leaseOptionsWallet: &leaseOptionsWallet{
			WalletController: &mock.WalletController{},
		},
		leases: []*wallet.ListLeasedOutputResult{{
			LockedOutput: &wtxmgr.LockedOutput{
				Outpoint:               outpoint,
				LockID:                 lockID,
				ReleaseAfterSpendConfs: 6,
			},
		}},
	}
	rpcServer, _, err := New(&Config{
		Wallet:              controller,
		CoinSelectionLocker: &mockCoinSelectionLocker{},
	})
	require.NoError(t, err)

	resp, err := rpcServer.LeaseOutput(t.Context(), &LeaseOutputRequest{
		Id: lockID[:],
		Outpoint: &lnrpc.OutPoint{
			TxidBytes:   make([]byte, 32),
			OutputIndex: outpoint.Index,
		},
		ExpirationSeconds: 60,
	})
	require.NoError(t, err)
	require.Equal(t, uint32(6), resp.ReleaseAfterSpendConfs)
	require.Equal(t, 1, controller.legacyCalls)
}

// TestLeaseOutputIgnoresRenewalDepthReadError verifies an informational depth
// lookup cannot prevent a successful legacy lease.
func TestLeaseOutputIgnoresRenewalDepthReadError(t *testing.T) {
	t.Parallel()

	lockID := wtxmgr.LockID{1}
	controller := &renewalWallet{
		leaseOptionsWallet: &leaseOptionsWallet{
			WalletController: &mock.WalletController{},
		},
		listErr: errors.New("list leases failed"),
	}
	rpcServer, _, err := New(&Config{
		Wallet:              controller,
		CoinSelectionLocker: &mockCoinSelectionLocker{},
	})
	require.NoError(t, err)

	resp, err := rpcServer.LeaseOutput(t.Context(), &LeaseOutputRequest{
		Id: lockID[:],
		Outpoint: &lnrpc.OutPoint{
			TxidBytes: make([]byte, 32),
		},
		ExpirationSeconds: 60,
	})
	require.NoError(t, err)
	require.Zero(t, resp.ReleaseAfterSpendConfs)
	require.Equal(t, 1, controller.legacyCalls)
}

// TestFundPsbtRequiresCustomLockID verifies confirmation-controlled input
// leases require an external, caller-specific owner ID.
func TestFundPsbtRequiresCustomLockID(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		lockID   []byte
		expected string
	}{
		{
			name: "missing",
			expected: "custom lock ID required for " +
				"confirmation-controlled",
		},
		{
			name:     "all zero",
			lockID:   make([]byte, 32),
			expected: "custom lock ID must not be all zeros",
		},
		{
			name:     "reserved internal",
			lockID:   chanfunding.LndInternalLockID[:],
			expected: "reserved custom lock ID cannot be used",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			server := &WalletKit{cfg: &Config{}}
			req := &FundPsbtRequest{
				InputReleaseAfterSpendConfs: 6,
			}
			req.CustomLockId = testCase.lockID

			_, err := server.FundPsbt(t.Context(), req)
			require.ErrorContains(t, err, testCase.expected)
		})
	}
}

// TestFundPsbtCoinSelect tests that the coin selection for a PSBT template
// works as expected.
func TestFundPsbtCoinSelect(t *testing.T) {
	t.Parallel()

	const fundAmt = 50_000
	var (
		p2wkhDustLimit = lnwallet.DustLimitForSize(input.P2WPKHSize)
		p2trDustLimit  = lnwallet.DustLimitForSize(input.P2TRSize)
		p2wkhScript, _ = input.WitnessPubKeyHash([]byte{})
		p2trScript, _  = txscript.PayToTaprootScript(
			&input.TaprootNUMSKey,
		)
	)

	makePacket := func(outs ...*wire.TxOut) *psbt.Packet {
		p := &psbt.Packet{
			UnsignedTx: &wire.MsgTx{},
		}

		for _, out := range outs {
			p.UnsignedTx.TxOut = append(p.UnsignedTx.TxOut, out)
			p.Outputs = append(p.Outputs, psbt.POutput{})
		}

		return p
	}
	updatePacket := func(p *psbt.Packet,
		f func(*psbt.Packet) *psbt.Packet) *psbt.Packet {

		return f(p)
	}
	calcFee := func(p2trIn, p2wkhIn, p2trOut, p2wkhOut int,
		dust btcutil.Amount) btcutil.Amount {

		estimator := input.TxWeightEstimator{}
		for i := 0; i < p2trIn; i++ {
			estimator.AddTaprootKeySpendInput(
				txscript.SigHashDefault,
			)
		}
		for i := 0; i < p2wkhIn; i++ {
			estimator.AddP2WKHInput()
		}
		for i := 0; i < p2trOut; i++ {
			estimator.AddP2TROutput()
		}
		for i := 0; i < p2wkhOut; i++ {
			estimator.AddP2WKHOutput()
		}

		weight := estimator.Weight()
		fee := chainfee.FeePerKwFloor.FeeForWeight(weight)

		return fee + dust
	}

	testCases := []struct {
		name        string
		utxos       []*lnwallet.Utxo
		packet      *psbt.Packet
		changeIndex int32
		changeType  chanfunding.ChangeAddressType
		feeRate     chainfee.SatPerKWeight

		// expectedUtxoIndexes is the list of utxo indexes that are
		// expected to be used for funding the psbt.
		expectedUtxoIndexes []int

		// expectChangeOutputIndex is the expected output index that is
		// returned from the tested method.
		expectChangeOutputIndex int32

		// expectedChangeOutputAmount is the expected final total amount
		// of the output marked as the change output. This will only be
		// checked if the expected amount is non-zero.
		expectedChangeOutputAmount btcutil.Amount

		// expectedFee is the total amount of fees paid by the funded
		// packet in bytes.
		expectedFee btcutil.Amount

		// maxFeeRatio is the maximum fee to total output amount ratio
		// that we consider valid.
		maxFeeRatio float64

		// expectedErr is the expected concrete error. If not nil, then
		// the error must match exactly.
		expectedErr error

		// expectedContainedErrStr is the expected string to be
		// contained in the returned error.
		expectedContainedErrStr string

		// expectedErrType is the expected error type. If not nil, then
		// the error must be of this type.
		expectedErrType error
	}{{
		name:  "no utxos",
		utxos: []*lnwallet.Utxo{},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:     -1,
		feeRate:         chainfee.FeePerKwFloor,
		maxFeeRatio:     chanfunding.DefaultMaxFeeRatio,
		expectedErrType: &chanfunding.ErrInsufficientFunds{},
	}, {
		name: "1 p2wpkh utxo, add p2wkh change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    100_000,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 1,
		expectedFee:             calcFee(0, 1, 1, 1, 0),
	}, {
		name: "1 p2wpkh utxo, add p2tr change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    100_000,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2TRChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 1,
		expectedFee:             calcFee(0, 1, 2, 0, 0),
	}, {
		name: "1 p2wpkh utxo, no change, exact amount",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt + 123,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: -1,
		expectedFee:             calcFee(0, 1, 1, 0, 0),
	}, {
		name: "1 p2wpkh utxo, no change, p2wpkh change dust to fee",
		utxos: []*lnwallet.Utxo{
			{
				Value: fundAmt + calcFee(
					0, 1, 1, 0, p2wkhDustLimit-1,
				),
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2WKHChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: -1,
		expectedFee:             calcFee(0, 1, 1, 0, p2wkhDustLimit-1),
	}, {
		name: "1 p2wpkh utxo, no change, p2tr change dust to fee",
		utxos: []*lnwallet.Utxo{
			{
				Value: fundAmt + calcFee(
					0, 1, 1, 0, p2trDustLimit-1,
				),
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2TRChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: -1,
		expectedFee:             calcFee(0, 1, 1, 0, p2trDustLimit-1),
	}, {
		name: "1 p2wpkh utxo, existing p2tr change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt + 50_000,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 1, 0, 0),
	}, {
		name: "1 p2wpkh utxo, existing p2wkh change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt + 50_000,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2wkhScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 0, 1, 0),
	}, {
		name: "1 p2wpkh utxo, existing p2wkh change, dust change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt + calcFee(0, 1, 0, 1, 0) + 50,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2wkhScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 0, 1, 0),
	}, {
		name: "1 p2wpkh + 1 p2tr utxo, existing p2tr input, existing " +
			"p2tr change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt / 2,
				PkScript: p2wkhScript,
			}, {
				Value:    fundAmt / 2,
				PkScript: p2trScript,
			},
		},
		packet: updatePacket(makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}), func(p *psbt.Packet) *psbt.Packet {
			p.UnsignedTx.TxIn = append(
				p.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash: chainhash.Hash{1, 2, 3},
					},
				},
			)
			p2TrDerivations := []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						&input.TaprootNUMSKey,
					),
					Bip32Path: []uint32{1, 2, 3},
				},
			}
			p.Inputs = append(p.Inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value:    1000,
					PkScript: p2trScript,
				},
				SighashType:            txscript.SigHashSingle,
				TaprootBip32Derivation: p2TrDerivations,
			})

			return p
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0, 1},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(2, 1, 1, 0, 0),
	}, {
		name: "1 p2wpkh + 1 p2tr utxo, existing p2tr input, add p2tr " +
			"change",
		utxos: []*lnwallet.Utxo{
			{
				Value:    fundAmt / 2,
				PkScript: p2wkhScript,
			}, {
				Value:    fundAmt / 2,
				PkScript: p2trScript,
			},
		},
		packet: updatePacket(makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}), func(p *psbt.Packet) *psbt.Packet {
			p.UnsignedTx.TxIn = append(
				p.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash: chainhash.Hash{1, 2, 3},
					},
				},
			)
			p2TrDerivations := []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						&input.TaprootNUMSKey,
					),
					Bip32Path: []uint32{1, 2, 3},
				},
			}
			p.Inputs = append(p.Inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value:    1000,
					PkScript: p2trScript,
				},
				SighashType:            txscript.SigHashSingle,
				TaprootBip32Derivation: p2TrDerivations,
			})

			return p
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2TRChangeAddress,
		expectedUtxoIndexes:     []int{0, 1},
		expectChangeOutputIndex: 1,
		expectedFee:             calcFee(2, 1, 2, 0, 0),
	}, {
		name: "large existing p2tr input, fee estimation p2wpkh " +
			"change",
		utxos: []*lnwallet.Utxo{},
		packet: updatePacket(makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}), func(p *psbt.Packet) *psbt.Packet {
			p.UnsignedTx.TxIn = append(
				p.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash: chainhash.Hash{1, 2, 3},
					},
				},
			)
			p2TrDerivations := []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						&input.TaprootNUMSKey,
					),
					Bip32Path: []uint32{1, 2, 3},
				},
			}
			p.Inputs = append(p.Inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value:    fundAmt * 3,
					PkScript: p2trScript,
				},
				TaprootBip32Derivation: p2TrDerivations,
			})

			return p
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2WKHChangeAddress,
		expectedUtxoIndexes:     []int{},
		expectChangeOutputIndex: 1,
		expectedChangeOutputAmount: fundAmt*3 - fundAmt -
			calcFee(1, 0, 1, 1, 0),
		expectedFee: calcFee(1, 0, 1, 1, 0),
	}, {
		name:  "large existing p2tr input, fee estimation no change",
		utxos: []*lnwallet.Utxo{},
		packet: updatePacket(makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}), func(p *psbt.Packet) *psbt.Packet {
			p.UnsignedTx.TxIn = append(
				p.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash: chainhash.Hash{1, 2, 3},
					},
				},
			)
			p2TrDerivations := []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						&input.TaprootNUMSKey,
					),
					Bip32Path: []uint32{1, 2, 3},
				},
			}
			p.Inputs = append(p.Inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value: fundAmt +
						int64(calcFee(1, 0, 1, 0, 0)),
					PkScript: p2trScript,
				},
				TaprootBip32Derivation: p2TrDerivations,
			})

			return p
		}),
		changeIndex:             -1,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.P2TRChangeAddress,
		expectedUtxoIndexes:     []int{},
		expectChangeOutputIndex: -1,
		expectedFee:             calcFee(1, 0, 1, 0, 0),
	}, {
		name: "1 p2wpkh utxo, existing p2wkh change, invalid fee ratio",
		utxos: []*lnwallet.Utxo{
			{
				Value:    250,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    50,
			PkScript: p2wkhScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 0, 1, 0),

		expectedContainedErrStr: "fee 0.00000111 BTC exceeds max fee " +
			"(0.00000027 BTC) on total output value",
	}, {
		name: "1 p2wpkh utxo, existing p2wkh change, negative feeratio",
		utxos: []*lnwallet.Utxo{
			{
				Value:    250,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    50,
			PkScript: p2wkhScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             chanfunding.DefaultMaxFeeRatio * (-1),
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 0, 1, 0),

		expectedContainedErrStr: "maxFeeRatio must be between 0.00 " +
			"and 5.00, got -0.20",
	}, {
		name: "1 p2wpkh utxo, existing p2wkh change, big fee ratio",
		utxos: []*lnwallet.Utxo{
			{
				Value:    250,
				PkScript: p2wkhScript,
			},
		},
		packet: makePacket(&wire.TxOut{
			Value:    50,
			PkScript: p2wkhScript,
		}),
		changeIndex:             0,
		feeRate:                 chainfee.FeePerKwFloor,
		maxFeeRatio:             0.85,
		changeType:              chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:     []int{0},
		expectChangeOutputIndex: 0,
		expectedFee:             calcFee(0, 1, 0, 1, 0),
	}, {
		name: "large existing p2tr input, fee estimation existing " +
			"change output",
		utxos: []*lnwallet.Utxo{},
		packet: updatePacket(makePacket(&wire.TxOut{
			Value:    fundAmt,
			PkScript: p2trScript,
		}), func(p *psbt.Packet) *psbt.Packet {
			p.UnsignedTx.TxIn = append(
				p.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash: chainhash.Hash{1, 2, 3},
					},
				},
			)
			p2TrDerivations := []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						&input.TaprootNUMSKey,
					),
					Bip32Path: []uint32{1, 2, 3},
				},
			}
			p.Inputs = append(p.Inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value:    fundAmt * 2,
					PkScript: p2trScript,
				},
				TaprootBip32Derivation: p2TrDerivations,
			})

			return p
		}),
		changeIndex:                0,
		feeRate:                    chainfee.FeePerKwFloor,
		maxFeeRatio:                chanfunding.DefaultMaxFeeRatio,
		changeType:                 chanfunding.ExistingChangeAddress,
		expectedUtxoIndexes:        []int{},
		expectChangeOutputIndex:    0,
		expectedChangeOutputAmount: fundAmt*2 - calcFee(1, 0, 1, 0, 0),
		expectedFee:                calcFee(1, 0, 1, 0, 0),
	}}

	for _, tc := range testCases {

		privKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		walletMock := &mock.WalletController{
			RootKey: privKey,
			Utxos:   tc.utxos,
		}
		rpcServer, _, err := New(&Config{
			Wallet:                walletMock,
			CoinSelectionLocker:   &mockCoinSelectionLocker{},
			CoinSelectionStrategy: wallet.CoinSelectionLargest,
		})
		require.NoError(t, err)

		t.Run(tc.name, func(tt *testing.T) {
			// To avoid our packet being mutated, we'll make a deep
			// copy of it, so we can still use the original in the
			// test case to compare the results to.
			var buf bytes.Buffer
			err := tc.packet.Serialize(&buf)
			require.NoError(tt, err)

			copiedPacket, err := psbt.NewFromRawBytes(&buf, false)
			require.NoError(tt, err)

			resp, err := rpcServer.fundPsbtCoinSelect(
				"", tc.changeIndex, copiedPacket, 0,
				tc.changeType, tc.feeRate,
				rpcServer.cfg.CoinSelectionStrategy,
				tc.maxFeeRatio, nil, 0, 0,
			)

			switch {
			case tc.expectedErr != nil:
				require.Error(tt, err)
				require.ErrorIs(tt, err, tc.expectedErr)

				return

			case tc.expectedErrType != nil:
				require.Error(tt, err)
				require.ErrorAs(tt, err, &tc.expectedErr)

				return
			case tc.expectedContainedErrStr != "":
				require.ErrorContains(
					tt, err, tc.expectedContainedErrStr,
				)

				return
			}

			require.NoError(tt, err)
			require.NotNil(tt, resp)

			resultPacket, err := psbt.NewFromRawBytes(
				bytes.NewReader(resp.FundedPsbt), false,
			)
			require.NoError(tt, err)
			resultTx := resultPacket.UnsignedTx

			expectedNumInputs := len(tc.expectedUtxoIndexes) +
				len(tc.packet.Inputs)
			require.Len(tt, resultPacket.Inputs, expectedNumInputs)
			require.Len(tt, resultTx.TxIn, expectedNumInputs)
			require.Equal(
				tt, tc.expectChangeOutputIndex,
				resp.ChangeOutputIndex,
			)

			fee, err := resultPacket.GetTxFee()
			require.NoError(tt, err)
			require.EqualValues(tt, tc.expectedFee, fee)

			if tc.expectedChangeOutputAmount != 0 {
				changeIdx := resp.ChangeOutputIndex
				require.GreaterOrEqual(tt, changeIdx, int32(-1))
				require.Less(
					tt, changeIdx,
					int32(len(resultTx.TxOut)),
				)

				changeOut := resultTx.TxOut[changeIdx]

				require.EqualValues(
					tt, tc.expectedChangeOutputAmount,
					changeOut.Value,
				)
			}
		})
	}
}

// hash01, hash02 and hashFF are ordered by their raw bytes as
// hash01 < hash02 < hashFF, even though the same is not true for the
// byte-reversed txid strings shown to users.
var (
	hash01 = chainhash.Hash{31: 0x01}
	hash02 = chainhash.Hash{31: 0x02}
	hashFF = chainhash.Hash{0: 0xff}
)

// newSortTestUtxos returns a fresh, deliberately unordered set of utxos that
// share amounts or confirmation counts, so the outpoint tie breaker is
// exercised.
func newSortTestUtxos() []*lnwallet.Utxo {
	return []*lnwallet.Utxo{
		{
			AddressType:   lnwallet.WitnessPubKey,
			Value:         3_000,
			Confirmations: 3,
			PkScript:      mock.CoinPkScript,
			OutPoint:      wire.OutPoint{Hash: hashFF, Index: 1},
		},
		{
			AddressType:   lnwallet.WitnessPubKey,
			Value:         1_000,
			Confirmations: 1,
			PkScript:      mock.CoinPkScript,
			OutPoint:      wire.OutPoint{Hash: hash02, Index: 9},
		},
		{
			AddressType:   lnwallet.WitnessPubKey,
			Value:         1_000,
			Confirmations: 9,
			PkScript:      mock.CoinPkScript,
			OutPoint:      wire.OutPoint{Hash: hash01, Index: 5},
		},
		{
			AddressType:   lnwallet.WitnessPubKey,
			Value:         2_000,
			Confirmations: 1,
			PkScript:      mock.CoinPkScript,
			OutPoint:      wire.OutPoint{Hash: hash02, Index: 2},
		},
		{
			AddressType:   lnwallet.WitnessPubKey,
			Value:         3_000,
			Confirmations: 7,
			PkScript:      mock.CoinPkScript,
			OutPoint:      wire.OutPoint{Hash: hash01, Index: 8},
		},
	}
}

// utxoKeys identifies the order of utxos as raw txid bytes plus output
// index.
func utxoKeys(t *testing.T, utxos []*lnwallet.Utxo) []string {
	t.Helper()

	keys := make([]string, len(utxos))
	for i, utxo := range utxos {
		keys[i] = fmt.Sprintf("%s:%d", utxo.Hash[:], utxo.Index)
	}

	return keys
}

func rpcUtxoKeys(t *testing.T, utxos []*lnrpc.Utxo) []string {
	t.Helper()

	keys := make([]string, len(utxos))
	for i, utxo := range utxos {
		op := utxo.Outpoint
		keys[i] = fmt.Sprintf("%s:%d", op.TxidBytes, op.OutputIndex)
	}

	return keys
}

// reverseUtxos returns a copy of utxos in reverse order.
func reverseUtxos(utxos []*lnwallet.Utxo) []*lnwallet.Utxo {
	reversed := make([]*lnwallet.Utxo, len(utxos))
	for i, utxo := range utxos {
		reversed[len(utxos)-1-i] = utxo
	}

	return reversed
}

// TestSortUtxos verifies that sortUtxos orders utxos by amount,
// confirmations or outpoint in ascending or descending direction, that
// equal primary keys are broken by the raw outpoint bytes and output index,
// that an empty result is handled, and that the resulting order does not
// depend on the input order.
func TestSortUtxos(t *testing.T) {
	t.Parallel()

	key01_5 := fmt.Sprintf("%s:5", hash01[:])
	key01_8 := fmt.Sprintf("%s:8", hash01[:])
	key02_2 := fmt.Sprintf("%s:2", hash02[:])
	key02_9 := fmt.Sprintf("%s:9", hash02[:])
	keyFF_1 := fmt.Sprintf("%s:1", hashFF[:])

	testCases := []struct {
		name      string
		field     ListUnspentSortField
		direction SortDirection
		expected  []string
	}{{
		name:      "amount ascending",
		field:     ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_AMOUNT,
		direction: SortDirection_SORT_DIRECTION_ASC,
		expected: []string{
			key01_5, key02_9, key02_2, key01_8, keyFF_1,
		},
	}, {
		name:      "amount descending",
		field:     ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_AMOUNT,
		direction: SortDirection_SORT_DIRECTION_DESC,
		expected: []string{
			key01_8, keyFF_1, key02_2, key01_5, key02_9,
		},
	}, {
		name:      "amount defaults to ascending without a direction",
		field:     ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_AMOUNT,
		direction: SortDirection_SORT_DIRECTION_UNSPECIFIED,
		expected: []string{
			key01_5, key02_9, key02_2, key01_8, keyFF_1,
		},
	}, {
		name:      "confirmations ascending",
		field:     ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_CONFIRMATIONS,
		direction: SortDirection_SORT_DIRECTION_ASC,
		expected: []string{
			key02_2, key02_9, keyFF_1, key01_8, key01_5,
		},
	}, {
		name:      "confirmations descending",
		field:     ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_CONFIRMATIONS,
		direction: SortDirection_SORT_DIRECTION_DESC,
		expected: []string{
			key01_5, key01_8, keyFF_1, key02_2, key02_9,
		},
	}, {
		name:      "outpoint ascending raw bytes then index",
		field:     ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_OUTPOINT,
		direction: SortDirection_SORT_DIRECTION_ASC,
		expected: []string{
			key01_5, key01_8, key02_2, key02_9, keyFF_1,
		},
	}, {
		name:      "outpoint descending",
		field:     ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_OUTPOINT,
		direction: SortDirection_SORT_DIRECTION_DESC,
		expected: []string{
			keyFF_1, key02_9, key02_2, key01_8, key01_5,
		},
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(tt *testing.T) {
			tt.Parallel()

			// Sort the unordered set and verify the expected
			// order.
			utxos := newSortTestUtxos()
			sortUtxos(utxos, tc.field, tc.direction)
			require.Equal(tt, tc.expected, utxoKeys(tt, utxos))

			// Sorting a differently ordered set containing the
			// same utxos must produce the exact same order.
			reversed := reverseUtxos(newSortTestUtxos())
			sortUtxos(reversed, tc.field, tc.direction)
			require.Equal(
				tt, tc.expected, utxoKeys(tt, reversed),
			)
		})
	}

	// Equal amounts and confirmations across multiple utxos are always
	// ordered by the raw outpoint transaction ID bytes and the output
	// index in ascending order.
	t.Run("equal amount and confirmations tie on outpoint", func(tt *testing.T) {
		tt.Parallel()

		utxos := []*lnwallet.Utxo{
			{
				Value:         1_000,
				Confirmations: 6,
				OutPoint:      wire.OutPoint{Hash: hashFF, Index: 3},
			},
			{
				Value:         1_000,
				Confirmations: 6,
				OutPoint:      wire.OutPoint{Hash: hashFF, Index: 1},
			},
			{
				Value:         1_000,
				Confirmations: 6,
				OutPoint:      wire.OutPoint{Hash: hash01, Index: 9},
			},
			{
				Value:         1_000,
				Confirmations: 6,
				OutPoint:      wire.OutPoint{Hash: hash01, Index: 2},
			},
		}

		sortUtxos(
			utxos, ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_AMOUNT,
			SortDirection_SORT_DIRECTION_ASC,
		)

		require.Equal(tt, []string{
			fmt.Sprintf("%s:2", hash01[:]),
			fmt.Sprintf("%s:9", hash01[:]),
			fmt.Sprintf("%s:1", hashFF[:]),
			fmt.Sprintf("%s:3", hashFF[:]),
		}, utxoKeys(tt, utxos))

		// In descending amount order the outpoint tie breaker still
		// remains ascending, only equal amounts stay grouped.
		descending := reverseUtxos(utxos)
		sortUtxos(
			descending, ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_AMOUNT,
			SortDirection_SORT_DIRECTION_DESC,
		)
		require.Equal(tt, utxoKeys(tt, utxos), utxoKeys(tt, descending))
	})

	// Sorting an empty set must be a no-op and must not panic.
	t.Run("empty result", func(tt *testing.T) {
		tt.Parallel()

		var utxos []*lnwallet.Utxo
		require.NotPanics(tt, func() {
			sortUtxos(
				utxos, ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_AMOUNT,
				SortDirection_SORT_DIRECTION_ASC,
			)
		})
		require.Empty(tt, utxos)
	})
}

// TestListUnspentSortValidation verifies that invalid sort parameters are
// rejected with explicit errors and that valid combinations are accepted.
func TestListUnspentSortValidation(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		field         ListUnspentSortField
		direction     SortDirection
		expectedError string
	}{{
		name:      "no sort parameters",
		field:     ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_UNSPECIFIED,
		direction: SortDirection_SORT_DIRECTION_UNSPECIFIED,
	}, {
		name:      "amount ascending",
		field:     ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_AMOUNT,
		direction: SortDirection_SORT_DIRECTION_ASC,
	}, {
		name:      "field without direction defaults to ascending",
		field:     ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_OUTPOINT,
		direction: SortDirection_SORT_DIRECTION_UNSPECIFIED,
	}, {
		name:          "unknown sort field",
		field:         ListUnspentSortField(99),
		direction:     SortDirection_SORT_DIRECTION_UNSPECIFIED,
		expectedError: "unknown sort_field",
	}, {
		name:          "unknown sort direction",
		field:         ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_AMOUNT,
		direction:     SortDirection(99),
		expectedError: "unknown sort_direction",
	}, {
		name:      "direction without field",
		field:     ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_UNSPECIFIED,
		direction: SortDirection_SORT_DIRECTION_ASC,
		expectedError: "sort_direction can only be set in combination " +
			"with sort_field",
	}, {
		name:      "descending direction without field",
		field:     ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_UNSPECIFIED,
		direction: SortDirection_SORT_DIRECTION_DESC,
		expectedError: "sort_direction can only be set in combination " +
			"with sort_field",
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(tt *testing.T) {
			tt.Parallel()

			err := validateListUnspentSort(tc.field, tc.direction)
			if tc.expectedError == "" {
				require.NoError(tt, err)

				return
			}

			require.ErrorContains(tt, err, tc.expectedError)
		})
	}
}

// newListUnspentTestServer creates a WalletKit RPC server backed by a mock
// wallet that returns the given utxos for any ListUnspentWitness call.
func newListUnspentTestServer(t *testing.T,
	utxos []*lnwallet.Utxo) *WalletKit {

	t.Helper()

	walletMock := &mock.WalletController{Utxos: utxos}
	rpcServer, _, err := New(&Config{
		Wallet:              walletMock,
		CoinSelectionLocker: &mockCoinSelectionLocker{},
		ChainParams:         &chaincfg.TestNet3Params,
	})
	require.NoError(t, err)

	return rpcServer
}

// TestListUnspentSorting exercises the full ListUnspent RPC, verifying that
// the response is ordered after filtering, that the default order is
// preserved without sort parameters and that parameter errors are
// returned.
func TestListUnspentSorting(t *testing.T) {
	t.Parallel()

	key01_5 := fmt.Sprintf("%s:5", hash01[:])
	key02_2 := fmt.Sprintf("%s:2", hash02[:])
	key02_9 := fmt.Sprintf("%s:9", hash02[:])
	key01_8 := fmt.Sprintf("%s:8", hash01[:])
	keyFF_1 := fmt.Sprintf("%s:1", hashFF[:])

	t.Run("preserves wallet order without sort parameters", func(tt *testing.T) {
		tt.Parallel()

		utxos := newSortTestUtxos()
		server := newListUnspentTestServer(tt, utxos)

		resp, err := server.ListUnspent(
			tt.Context(), &ListUnspentRequest{},
		)
		require.NoError(tt, err)
		require.Equal(
			tt, utxoKeys(tt, utxos), rpcUtxoKeys(tt, resp.Utxos),
		)
	})

	t.Run("orders by amount ascending", func(tt *testing.T) {
		tt.Parallel()

		server := newListUnspentTestServer(tt, newSortTestUtxos())

		resp, err := server.ListUnspent(tt.Context(), &ListUnspentRequest{
			SortField: ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_AMOUNT,
		})
		require.NoError(tt, err)
		require.Equal(tt, []string{
			key01_5, key02_9, key02_2, key01_8, keyFF_1,
		}, rpcUtxoKeys(tt, resp.Utxos))
	})

	t.Run("orders by confirmations descending", func(tt *testing.T) {
		tt.Parallel()

		server := newListUnspentTestServer(tt, newSortTestUtxos())

		resp, err := server.ListUnspent(tt.Context(), &ListUnspentRequest{
			SortField:     ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_CONFIRMATIONS,
			SortDirection: SortDirection_SORT_DIRECTION_DESC,
		})
		require.NoError(tt, err)
		require.Equal(tt, []string{
			key01_5, key01_8, keyFF_1, key02_2, key02_9,
		}, rpcUtxoKeys(tt, resp.Utxos))
	})

	t.Run("orders by outpoint ascending", func(tt *testing.T) {
		tt.Parallel()

		server := newListUnspentTestServer(tt, newSortTestUtxos())

		resp, err := server.ListUnspent(tt.Context(), &ListUnspentRequest{
			SortField:     ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_OUTPOINT,
			SortDirection: SortDirection_SORT_DIRECTION_ASC,
		})
		require.NoError(tt, err)
		require.Equal(tt, []string{
			key01_5, key01_8, key02_2, key02_9, keyFF_1,
		}, rpcUtxoKeys(tt, resp.Utxos))
	})

	t.Run("empty result", func(tt *testing.T) {
		tt.Parallel()

		server := newListUnspentTestServer(
			tt, []*lnwallet.Utxo{},
		)

		resp, err := server.ListUnspent(tt.Context(), &ListUnspentRequest{
			SortField: ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_AMOUNT,
		})
		require.NoError(tt, err)
		require.Empty(tt, resp.Utxos)
	})

	errorCases := []struct {
		name          string
		req           *ListUnspentRequest
		expectedError string
	}{{
		name: "unknown sort field",
		req: &ListUnspentRequest{
			SortField: ListUnspentSortField(99),
		},
		expectedError: "unknown sort_field",
	}, {
		name: "unknown sort direction",
		req: &ListUnspentRequest{
			SortField:     ListUnspentSortField_LIST_UNSPENT_SORT_FIELD_AMOUNT,
			SortDirection: SortDirection(99),
		},
		expectedError: "unknown sort_direction",
	}, {
		name: "direction without field",
		req: &ListUnspentRequest{
			SortDirection: SortDirection_SORT_DIRECTION_ASC,
		},
		expectedError: "sort_direction can only be set in combination " +
			"with sort_field",
	}}

	for _, tc := range errorCases {
		t.Run(tc.name, func(tt *testing.T) {
			tt.Parallel()

			server := newListUnspentTestServer(tt, newSortTestUtxos())

			_, err := server.ListUnspent(tt.Context(), tc.req)
			require.ErrorContains(tt, err, tc.expectedError)
		})
	}
}
