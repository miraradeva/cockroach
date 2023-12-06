// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package rangefeed

import (
	"context"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/concurrency/isolation"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/storage/enginepb"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/stretchr/testify/require"
)

// smallEngineBlocks configures Pebble with a block size of 1 byte, to provoke
// bugs in time-bound iterators.
var smallEngineBlocks = util.ConstantWithMetamorphicTestBool("small-engine-blocks", false)

// TODO(erikgrinaker): This should be migrated to a data-driven test harness for
// end-to-end rangefeed testing, with more exhaustive test cases. See:
// https://github.com/cockroachdb/cockroach/issues/82715
//
// For now, see rangefeed_external_test.go for rudimentary range key tests.
//
// To invoke and compare on the numRangeKeys dimension:
//
//	go test ./pkg/kv/kvserver/rangefeed/ -run - -count 10 -bench BenchmarkCatchUpScan 2>&1 | tee bench.txt
//	for flavor in numRangeKeys=0 numRangeKeys=1 numRangeKeys=100; do grep -E "${flavor}[^0-9]+" bench.txt | sed -E "s/${flavor}+/X/" > $flavor.txt; done
//	benchstat numRangeKeys\={0,1}.txt
//	benchstat numRangeKeys\={0,100}.txt
func TestCatchupScan(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()

	var (
		testKey1 = roachpb.Key("/db1")
		testKey2 = roachpb.Key("/db2")

		testValue1 = roachpb.MakeValueFromString("val1")
		testValue2 = roachpb.MakeValueFromString("val2")
		testValue3 = roachpb.MakeValueFromString("val3")
		testValue4 = roachpb.MakeValueFromString("val4")

		ts1 = hlc.Timestamp{WallTime: 1, Logical: 0}
		ts2 = hlc.Timestamp{WallTime: 2, Logical: 0}
		ts3 = hlc.Timestamp{WallTime: 3, Logical: 0}
		ts4 = hlc.Timestamp{WallTime: 4, Logical: 0}
		ts5 = hlc.Timestamp{WallTime: 4, Logical: 0}
		ts6 = hlc.Timestamp{WallTime: 5, Logical: 0}
	)

	makeTxn := func(key roachpb.Key, val roachpb.Value, ts hlc.Timestamp,
	) (roachpb.Transaction, roachpb.Value) {
		txnID := uuid.MakeV4()
		txnMeta := enginepb.TxnMeta{
			Key:            key,
			ID:             txnID,
			Epoch:          1,
			WriteTimestamp: ts,
		}
		return roachpb.Transaction{
				TxnMeta:       txnMeta,
				ReadTimestamp: ts,
			}, roachpb.Value{
				RawBytes: val.RawBytes,
			}
	}

	makeKTV := func(key roachpb.Key, ts hlc.Timestamp, value roachpb.Value) storage.MVCCKeyValue {
		return storage.MVCCKeyValue{Key: storage.MVCCKey{Key: key, Timestamp: ts}, Value: value.RawBytes}
	}

	// testKey1 has an intent and provisional value that will be skipped. Both
	// testKey1 and testKey2 have a value that is older than what we need with
	// the catchup scan, but will be read if a diff is desired.
	kv1_1_1 := makeKTV(testKey1, ts1, testValue1)
	kv1_2_2 := makeKTV(testKey1, ts2, testValue2)
	kv1_3_3 := makeKTV(testKey1, ts3, testValue3)
	kv1_4_4 := makeKTV(testKey1, ts4, testValue4)
	txn1, val1 := makeTxn(testKey1, testValue4, ts4)
	kv2_1_1 := makeKTV(testKey2, ts1, testValue1)
	kv2_2_2 := makeKTV(testKey2, ts2, testValue2)
	kv2_5_3 := makeKTV(testKey2, ts5, testValue3)
	kv2_6_4 := makeKTV(testKey2, ts6, testValue4)
	txn2, val2 := makeTxn(testKey2, testValue4, ts6)
	txn2.OmitInRangefeeds = true

	eng := storage.NewDefaultInMemForTesting(storage.If(smallEngineBlocks, storage.BlockSize(1)))
	defer eng.Close()
	// Put with no intent.
	for _, kv := range []storage.MVCCKeyValue{kv1_1_1, kv1_2_2, kv1_3_3, kv2_1_1, kv2_2_2, kv2_5_3} {
		v := roachpb.Value{RawBytes: kv.Value}
		if _, err := storage.MVCCPut(
			ctx, eng, kv.Key.Key, kv.Key.Timestamp, v, storage.MVCCWriteOptions{},
		); err != nil {
			t.Fatal(err)
		}
	}
	// Put with an intent.
	if _, err := storage.MVCCPut(
		ctx, eng, kv1_4_4.Key.Key, txn1.ReadTimestamp, val1, storage.MVCCWriteOptions{Txn: &txn1},
	); err != nil {
		t.Fatal(err)
	}
	// Transactional put with OmitInRangefeeds = true.
	if _, err := storage.MVCCPut(
		ctx, eng, kv2_6_4.Key.Key, txn2.ReadTimestamp, val2, storage.MVCCWriteOptions{Txn: &txn2},
	); err != nil {
		t.Fatal(err)
	}
	// Resolve the intent.
	intent := roachpb.LockUpdate{Txn: txn2.TxnMeta, Span: roachpb.Span{Key: kv2_6_4.Key.Key}, Status: roachpb.COMMITTED}
	if ok, _, _, _, err := storage.MVCCResolveWriteIntent(
		ctx, eng, nil, intent, storage.MVCCResolveWriteIntentOptions{}); err != nil || !ok {
		t.Fatal(err)
	}
	testutils.RunTrueAndFalse(t, "withDiff", func(t *testing.T, withDiff bool) {
		span := roachpb.Span{Key: testKey1, EndKey: roachpb.KeyMax}
		iter, err := NewCatchUpIterator(ctx, eng, span, ts1, nil, nil)
		require.NoError(t, err)
		defer iter.Close()
		var events []kvpb.RangeFeedValue
		// ts1 here is exclusive, so we do not want the versions at ts1.
		require.NoError(t, iter.CatchUpScan(ctx, func(e *kvpb.RangeFeedEvent) error {
			events = append(events, *e.Val)
			return nil
		}, withDiff))
		require.Equal(t, 4, len(events))
		checkEquality := func(
			kv storage.MVCCKeyValue, prevKV storage.MVCCKeyValue, event kvpb.RangeFeedValue) {
			require.Equal(t, string(kv.Key.Key), string(event.Key))
			require.Equal(t, kv.Key.Timestamp, event.Value.Timestamp)
			require.Equal(t, string(kv.Value), string(event.Value.RawBytes))
			if withDiff {
				// TODO(sumeer): uncomment after clarifying CatchUpScan behavior.
				// require.Equal(t, prevKV.Key.Timestamp, event.PrevValue.Timestamp)
				require.Equal(t, string(prevKV.Value), string(event.PrevValue.RawBytes))
			} else {
				require.Equal(t, hlc.Timestamp{}, event.PrevValue.Timestamp)
				require.Equal(t, 0, len(event.PrevValue.RawBytes))
			}
		}
		checkEquality(kv1_2_2, kv1_1_1, events[0])
		checkEquality(kv1_3_3, kv1_2_2, events[1])
		checkEquality(kv2_2_2, kv2_1_1, events[2])
		checkEquality(kv2_5_3, kv2_2_2, events[3])
	})
}

func TestCatchupScanInlineError(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	eng := storage.NewDefaultInMemForTesting(storage.If(smallEngineBlocks, storage.BlockSize(1)))
	defer eng.Close()

	// Write an inline value.
	_, err := storage.MVCCPut(ctx, eng, roachpb.Key("inline"), hlc.Timestamp{}, roachpb.MakeValueFromString("foo"), storage.MVCCWriteOptions{})
	require.NoError(t, err)

	// Run a catchup scan across the span and watch it error.
	span := roachpb.Span{Key: keys.LocalMax, EndKey: keys.MaxKey}
	iter, err := NewCatchUpIterator(ctx, eng, span, hlc.Timestamp{}, nil, nil)
	require.NoError(t, err)
	defer iter.Close()

	err = iter.CatchUpScan(ctx, nil, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unexpected inline value")
}

func TestCatchupScanSeesOldIntent(t *testing.T) {
	defer leaktest.AfterTest(t)()
	// Regression test for [#85886]. When with-diff is specified, the iterator may
	// be positioned on an intent that is outside the time bounds. When we read
	// the intent and want to load the version, we must make sure to ignore time
	// bounds, or we'll see a wholly unrelated version.
	//
	// [#85886]: https://github.com/cockroachdb/cockroach/issues/85886

	ctx := context.Background()
	eng := storage.NewDefaultInMemForTesting(storage.If(smallEngineBlocks, storage.BlockSize(1)))
	defer eng.Close()

	// b -> version @ 1100 (visible)
	// d -> intent @ 990   (iterator will be positioned here because of with-diff option)
	// e -> version @ 1100
	tsCutoff := hlc.Timestamp{WallTime: 1000} // the lower bound of the catch-up scan
	tsIntent := tsCutoff.Add(-10, 0)          // the intent is below the lower bound
	tsVersionInWindow := tsCutoff.Add(10, 0)  // an unrelated version is above the lower bound

	_, err := storage.MVCCPut(ctx, eng, roachpb.Key("b"),
		tsVersionInWindow, roachpb.MakeValueFromString("foo"), storage.MVCCWriteOptions{})
	require.NoError(t, err)

	txn := roachpb.MakeTransaction("foo", roachpb.Key("d"), isolation.Serializable, roachpb.NormalUserPriority, tsIntent, 100, 0, 0, false /* omitInRangefeeds */)
	_, err = storage.MVCCPut(ctx, eng, roachpb.Key("d"),
		tsIntent, roachpb.MakeValueFromString("intent"), storage.MVCCWriteOptions{Txn: &txn})
	require.NoError(t, err)

	_, err = storage.MVCCPut(ctx, eng, roachpb.Key("e"),
		tsVersionInWindow, roachpb.MakeValueFromString("bar"), storage.MVCCWriteOptions{})
	require.NoError(t, err)

	// Run a catchup scan across the span and watch it succeed.
	span := roachpb.Span{Key: keys.LocalMax, EndKey: keys.MaxKey}
	iter, err := NewCatchUpIterator(ctx, eng, span, tsCutoff, nil, nil)
	require.NoError(t, err)
	defer iter.Close()

	keys := map[string]struct{}{}
	require.NoError(t, iter.CatchUpScan(ctx, func(e *kvpb.RangeFeedEvent) error {
		keys[string(e.Val.Key)] = struct{}{}
		return nil
	}, true /* withDiff */))
	require.Equal(t, map[string]struct{}{
		"b": {},
		"e": {},
	}, keys)
}
