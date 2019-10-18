// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package batcheval

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/col/colconv"
	"github.com/cockroachdb/cockroach/pkg/col/coldb"
	"github.com/cockroachdb/cockroach/pkg/col/colengine"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage/batcheval/result"
	"github.com/cockroachdb/cockroach/pkg/storage/engine"
	"github.com/cockroachdb/cockroach/pkg/storage/engine/enginepb"
	"github.com/cockroachdb/cockroach/pkg/storage/spanset"
	"github.com/cockroachdb/cockroach/pkg/storage/storagepb"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/kr/pretty"
	"github.com/pkg/errors"
)

func init() {
	RegisterCommand(roachpb.AddSSTable, DefaultDeclareKeys, EvalAddSSTable)
}

// EvalAddSSTable evaluates an AddSSTable command.
func EvalAddSSTable(
	ctx context.Context, batch engine.ReadWriter, cArgs CommandArgs, _ roachpb.Response,
) (_ result.Result, retErr error) {
	defer func() {
		if retErr != nil {
			log.Infof(ctx, "WIP EvalAddSSTable failed %+v", retErr)
		}
	}()
	args := cArgs.Args.(*roachpb.AddSSTableRequest)
	h := cArgs.Header
	ms := cArgs.Stats
	mvccStartKey, mvccEndKey := engine.MVCCKey{Key: args.Key}, engine.MVCCKey{Key: args.EndKey}

	// TODO(tschottdorf): restore the below in some form (gets in the way of testing).
	// _, span := tracing.ChildSpan(ctx, fmt.Sprintf("AddSSTable [%s,%s)", args.Key, args.EndKey))
	// defer tracing.FinishSpan(span)
	log.Eventf(ctx, "evaluating AddSSTable [%s,%s)", mvccStartKey.Key, mvccEndKey.Key)

	// IMPORT INTO should not proceed if any KVs from the SST shadow existing data
	// entries - #38044.
	var skippedKVStats enginepb.MVCCStats
	var err error
	if args.DisallowShadowing {
		if skippedKVStats, err = checkForKeyCollisions(ctx, batch, mvccStartKey, mvccEndKey, args.Data); err != nil {
			return result.Result{}, errors.Wrap(err, "checking for key collisions")
		}
	}

	// Verify that the keys in the sstable are within the range specified by the
	// request header, and if the request did not include pre-computed stats,
	// compute the expected MVCC stats delta of ingesting the SST.
	dataIter, err := engine.NewMemSSTIterator(args.Data, true)
	if err != nil {
		return result.Result{}, err
	}
	defer dataIter.Close()

	// Check that the first key is in the expected range.
	dataIter.Seek(engine.MVCCKey{Key: keys.MinKey})
	ok, err := dataIter.Valid()
	if err != nil {
		return result.Result{}, err
	} else if ok {
		if unsafeKey := dataIter.UnsafeKey(); unsafeKey.Less(mvccStartKey) {
			return result.Result{}, errors.Errorf("first key %s not in request range [%s,%s)",
				unsafeKey.Key, mvccStartKey.Key, mvccEndKey.Key)
		}
	}

	// Get the MVCCStats for the SST being ingested.
	var stats enginepb.MVCCStats
	if args.MVCCStats != nil {
		stats = *args.MVCCStats
	}

	// Stats are computed on-the-fly when shadowing of keys is disallowed. If we
	// took the fast path and race is enabled, assert the stats were correctly
	// computed.
	verifyFastPath := args.DisallowShadowing && util.RaceEnabled
	if args.MVCCStats == nil || verifyFastPath {
		log.VEventf(ctx, 2, "computing MVCCStats for SSTable [%s,%s)", mvccStartKey.Key, mvccEndKey.Key)

		computed, err := engine.ComputeStatsGo(dataIter, mvccStartKey, mvccEndKey, h.Timestamp.WallTime)
		if err != nil {
			return result.Result{}, errors.Wrap(err, "computing SSTable MVCC stats")
		}

		if verifyFastPath {
			// Update the timestamp to that of the recently computed stats to get the
			// diff passing.
			stats.LastUpdateNanos = computed.LastUpdateNanos
			if !stats.Equal(computed) {
				log.Fatalf(ctx, "fast-path MVCCStats computation gave wrong result: diff(fast, computed) = %s",
					pretty.Diff(stats, computed))
			}
		}
		stats = computed
	}

	dataIter.Seek(mvccEndKey)
	ok, err = dataIter.Valid()
	if err != nil {
		return result.Result{}, err
	} else if ok {
		return result.Result{}, errors.Errorf("last key %s not in request range [%s,%s)",
			dataIter.UnsafeKey(), mvccStartKey.Key, mvccEndKey.Key)
	}

	// The above MVCCStats represents what is in this new SST.
	//
	// *If* the keys in the SST do not conflict with keys currently in this range,
	// then adding the stats for this SST to the range stats should yield the
	// correct overall stats.
	//
	// *However*, if the keys in this range *do* overlap with keys already in this
	// range, then adding the SST semantically *replaces*, rather than adds, those
	// keys, and the net effect on the stats is not so simple.
	//
	// To perfectly compute the correct net stats, you could a) determine the
	// stats for the span of the existing range that this SST covers and subtract
	// it from the range's stats, then b) use a merging iterator that reads from
	// the SST and then underlying range and compute the stats of that merged
	// span, and then add those stats back in. That would result in correct stats
	// that reflect the merging semantics when the SST "shadows" an existing key.
	//
	// If the underlying range is mostly empty, this isn't terribly expensive --
	// computing the existing stats to subtract is cheap, as there is little or no
	// existing data to traverse and b) is also pretty cheap -- the merging
	// iterator can quickly iterate the in-memory SST.
	//
	// However, if the underlying range is _not_ empty, then this is not cheap:
	// recomputing its stats involves traversing lots of data, and iterating the
	// merged iterator has to constantly go back and forth to the RocksDB-backed
	// (cgo) iterator.
	//
	// If we assume that most SSTs don't shadow too many keys, then the error of
	// simply adding the SST stats directly to the range stats is minimal. In the
	// worst-case, when we retry a whole SST, then it could be overcounting the
	// entire file, but we can hope that that is rare. In the worst case, it may
	// cause splitting an under-filled range that would later merge when the
	// over-count is fixed.
	//
	// We can indicate that these stats contain this estimation using the flag in
	// the MVCC stats so that later re-computations will not be surprised to find
	// any discrepancies.
	//
	// Callers can trigger such a re-computation to fixup any discrepancies (and
	// remove the ContainsEstimates flag) after they are done ingesting files by
	// sending an explicit recompute.
	//
	// There is a significant performance win to be achieved by ensuring that the
	// stats computed are not estimates as it prevents recompuation on splits.
	// Running AddSSTable with disallowShadowing=true gets us close to this as we
	// do not allow colliding keys to be ingested. However, in the situation that
	// two SSTs have KV(s) which "perfectly" shadow an existing key (equal ts and
	// value), we do not consider this a collision. While the KV would just
	// overwrite the existing data, the stats would be added to the cumulative
	// stats of the AddSSTable command, causing a double count for such KVs.
	// Therfore, we compute the stats for these "skipped" KVs on-the-fly while
	// checking for the collision condition in C++ and subtract them from the
	// stats of the SST being ingested before adding them to the running
	// cumulative for this command. These stats can then be marked as accurate.
	if args.DisallowShadowing {
		stats.Subtract(skippedKVStats)
	}
	stats.ContainsEstimates = !args.DisallowShadowing
	ms.Add(stats)

	log.Infof(ctx, "AddSSTable %s", args.Span())
	columnarNamespace := uint64(0) // WIP use the namespace on the read side too
	schemaer := cArgs.EvalCtx.GetSchemaProvider()
	columnarData, schema, err := colconv.SSTableToColumnar(
		ctx, schemaer, args.Span(), args.Data)
	if err != nil {
		return result.Result{}, err
	}

	return result.Result{
		Replicated: storagepb.ReplicatedEvalResult{
			AddSSTable: &storagepb.ReplicatedEvalResult_AddSSTable{
				Data:  args.Data,
				CRC32: util.CRC32(args.Data),
			},
			ColumnarData: &colengine.DeterministicData{
				Namespace: coldb.NamespaceID(columnarNamespace),
				Schema:    *schema,
				Data:      columnarData,
			},
		},
	}, nil
}

func checkForKeyCollisions(
	ctx context.Context,
	batch engine.ReadWriter,
	mvccStartKey engine.MVCCKey,
	mvccEndKey engine.MVCCKey,
	data []byte,
) (enginepb.MVCCStats, error) {
	// We could get a spansetBatch so fetch the underlying rocksDBBatchEngine as
	// we need access to the underlying C.DBIterator later, and the
	// dbIteratorGetter is not implemented by a spansetBatch.
	rocksDBEngine := spanset.GetDBEngine(batch, roachpb.Span{Key: mvccStartKey.Key, EndKey: mvccEndKey.Key})

	emptyMVCCStats := enginepb.MVCCStats{}

	// Create iterator over the existing data.
	existingDataIter := rocksDBEngine.NewIterator(engine.IterOptions{UpperBound: mvccEndKey.Key})
	defer existingDataIter.Close()
	existingDataIter.Seek(mvccStartKey)
	if ok, err := existingDataIter.Valid(); err != nil {
		return emptyMVCCStats, errors.Wrap(err, "checking for key collisions")
	} else if !ok {
		// Target key range is empty, so it is safe to ingest.
		return emptyMVCCStats, nil
	}

	// Create a C++ iterator over the SST being added. This iterator is used to
	// perform a check for key collisions between the SST being ingested, and the
	// exisiting data. As the collision check is in C++ we are unable to use a
	// pure go iterator as in verifySSTable.
	//
	// TODO(adityamaru): reuse this iterator in verifySSTable.
	sst := engine.MakeRocksDBSstFileReader()
	defer sst.Close()

	if err := sst.IngestExternalFile(data); err != nil {
		return emptyMVCCStats, err
	}
	sstIterator := sst.NewIterator(engine.IterOptions{UpperBound: mvccEndKey.Key})
	defer sstIterator.Close()
	sstIterator.Seek(mvccStartKey)
	if ok, err := sstIterator.Valid(); err != nil || !ok {
		return emptyMVCCStats, errors.Wrap(err, "checking for key collisions")
	}

	skippedKVStats, checkErr := engine.CheckForKeyCollisions(existingDataIter, sstIterator)
	return skippedKVStats, checkErr
}
