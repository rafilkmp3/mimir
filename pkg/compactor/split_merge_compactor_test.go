// SPDX-License-Identifier: AGPL-3.0-only

package compactor

import (
	"context"
	"io/ioutil"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/grafana/dskit/flagext"
	"github.com/grafana/dskit/services"
	"github.com/grafana/dskit/test"
	"github.com/oklog/ulid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thanos-io/thanos/pkg/block"
	"github.com/thanos-io/thanos/pkg/block/metadata"
	"github.com/thanos-io/thanos/pkg/objstore"

	"github.com/grafana/mimir/pkg/storage/bucket"
	mimir_tsdb "github.com/grafana/mimir/pkg/storage/tsdb"
)

func TestMultitenantCompactor_ShouldSupportSplitAndMergeCompactor(t *testing.T) {
	t.Parallel()

	const (
		userID     = "user-1"
		numShards  = 2
		numSeries  = 100
		blockRange = 2 * time.Hour
	)

	var (
		blockRangeMillis = blockRange.Milliseconds()
		compactionRanges = mimir_tsdb.DurationList{blockRange, 2 * blockRange, 4 * blockRange}
	)

	externalLabels := func(shardID string) map[string]string {
		labels := map[string]string{
			mimir_tsdb.TenantIDExternalLabel: userID,
		}

		if shardID != "" {
			labels[ShardIDLabelName] = shardID
		}
		return labels
	}

	tests := map[string]struct {
		setup func(t *testing.T, bkt objstore.Bucket) []metadata.Meta
	}{
		"overlapping blocks matching the 1st compaction range should be merged and split": {
			setup: func(t *testing.T, bkt objstore.Bucket) []metadata.Meta {
				block1 := createTSDBBlock(t, bkt, userID, blockRangeMillis, 2*blockRangeMillis, numSeries, externalLabels(""))
				block2 := createTSDBBlock(t, bkt, userID, blockRangeMillis, 2*blockRangeMillis, numSeries, externalLabels(""))

				return []metadata.Meta{
					{
						BlockMeta: tsdb.BlockMeta{
							MinTime: 1 * blockRangeMillis,
							MaxTime: 2 * blockRangeMillis,
							Compaction: tsdb.BlockMetaCompaction{
								Sources: []ulid.ULID{block1, block2},
							},
						},
						Thanos: metadata.Thanos{
							Labels: map[string]string{
								mimir_tsdb.TenantIDExternalLabel: userID,
								ShardIDLabelName:                 "1_of_2",
							},
						},
					}, {
						BlockMeta: tsdb.BlockMeta{
							MinTime: 1 * blockRangeMillis,
							MaxTime: 2 * blockRangeMillis,
							Compaction: tsdb.BlockMetaCompaction{
								Sources: []ulid.ULID{block1, block2},
							},
						},
						Thanos: metadata.Thanos{
							Labels: map[string]string{
								mimir_tsdb.TenantIDExternalLabel: userID,
								ShardIDLabelName:                 "2_of_2",
							},
						},
					},
				}
			},
		},
		"overlapping blocks matching the beginning of the 1st compaction range should be merged and split": {
			setup: func(t *testing.T, bkt objstore.Bucket) []metadata.Meta {
				block1 := createTSDBBlock(t, bkt, userID, 0, (5 * time.Minute).Milliseconds(), numSeries, externalLabels(""))
				block2 := createTSDBBlock(t,
					bkt,
					userID,
					time.Minute.Milliseconds(),
					(7 * time.Minute).Milliseconds(),
					numSeries,
					externalLabels(""))

				// Add another block as "most recent one" otherwise the previous blocks are not compacted
				// because the most recent blocks must cover the full range to be compacted.
				block3 := createTSDBBlock(t,
					bkt,
					userID,
					blockRangeMillis,
					blockRangeMillis+time.Minute.Milliseconds(),
					numSeries,
					externalLabels(""))

				return []metadata.Meta{
					{
						BlockMeta: tsdb.BlockMeta{
							MinTime: 0,
							MaxTime: (7 * time.Minute).Milliseconds(),
							Compaction: tsdb.BlockMetaCompaction{
								Sources: []ulid.ULID{block1, block2},
							},
						},
						Thanos: metadata.Thanos{
							Labels: map[string]string{
								mimir_tsdb.TenantIDExternalLabel: userID,
								ShardIDLabelName:                 "1_of_2",
							},
						},
					}, {
						BlockMeta: tsdb.BlockMeta{
							MinTime: 0,
							MaxTime: (7 * time.Minute).Milliseconds(),
							Compaction: tsdb.BlockMetaCompaction{
								Sources: []ulid.ULID{block1, block2},
							},
						},
						Thanos: metadata.Thanos{
							Labels: map[string]string{
								mimir_tsdb.TenantIDExternalLabel: userID,
								ShardIDLabelName:                 "2_of_2",
							},
						},
					}, {
						// Not compacted.
						BlockMeta: tsdb.BlockMeta{
							MinTime: blockRangeMillis,
							MaxTime: blockRangeMillis + time.Minute.Milliseconds(),
							Compaction: tsdb.BlockMetaCompaction{
								Sources: []ulid.ULID{block3},
							},
						},
						Thanos: metadata.Thanos{
							Labels: map[string]string{
								mimir_tsdb.TenantIDExternalLabel: userID,
							},
						},
					},
				}
			},
		},
		"non-overlapping blocks matching the beginning of the 1st compaction range (without gaps) should be merged and split": {
			setup: func(t *testing.T, bkt objstore.Bucket) []metadata.Meta {
				block1 := createTSDBBlock(t, bkt, userID, 0, (5 * time.Minute).Milliseconds(), numSeries, externalLabels(""))
				block2 := createTSDBBlock(t,
					bkt,
					userID,
					(5 * time.Minute).Milliseconds(),
					(10 * time.Minute).Milliseconds(),
					numSeries,
					externalLabels(""))

				// Add another block as "most recent one" otherwise the previous blocks are not compacted
				// because the most recent blocks must cover the full range to be compacted.
				block3 := createTSDBBlock(t,
					bkt,
					userID,
					blockRangeMillis,
					blockRangeMillis+time.Minute.Milliseconds(),
					numSeries,
					externalLabels(""))

				return []metadata.Meta{
					{
						BlockMeta: tsdb.BlockMeta{
							MinTime: 0,
							MaxTime: (10 * time.Minute).Milliseconds(),
							Compaction: tsdb.BlockMetaCompaction{
								Sources: []ulid.ULID{block1, block2},
							},
						},
						Thanos: metadata.Thanos{
							Labels: map[string]string{
								mimir_tsdb.TenantIDExternalLabel: userID,
								ShardIDLabelName:                 "1_of_2",
							},
						},
					}, {
						BlockMeta: tsdb.BlockMeta{
							MinTime: 0,
							MaxTime: (10 * time.Minute).Milliseconds(),
							Compaction: tsdb.BlockMetaCompaction{
								Sources: []ulid.ULID{block1, block2},
							},
						},
						Thanos: metadata.Thanos{
							Labels: map[string]string{
								mimir_tsdb.TenantIDExternalLabel: userID,
								ShardIDLabelName:                 "2_of_2",
							},
						},
					}, {
						// Not compacted.
						BlockMeta: tsdb.BlockMeta{
							MinTime: blockRangeMillis,
							MaxTime: blockRangeMillis + time.Minute.Milliseconds(),
							Compaction: tsdb.BlockMetaCompaction{
								Sources: []ulid.ULID{block3},
							},
						},
						Thanos: metadata.Thanos{
							Labels: map[string]string{
								mimir_tsdb.TenantIDExternalLabel: userID,
							},
						},
					},
				}
			},
		},
		"non-overlapping blocks matching the beginning of the 1st compaction range (with gaps) should be merged and split": {
			setup: func(t *testing.T, bkt objstore.Bucket) []metadata.Meta {
				block1 := createTSDBBlock(t, bkt, userID, 0, (5 * time.Minute).Milliseconds(), numSeries, externalLabels(""))
				block2 := createTSDBBlock(t,
					bkt,
					userID,
					(7 * time.Minute).Milliseconds(),
					(10 * time.Minute).Milliseconds(),
					numSeries,
					externalLabels(""))

				// Add another block as "most recent one" otherwise the previous blocks are not compacted
				// because the most recent blocks must cover the full range to be compacted.
				block3 := createTSDBBlock(t,
					bkt,
					userID,
					blockRangeMillis,
					blockRangeMillis+time.Minute.Milliseconds(),
					numSeries,
					externalLabels(""))

				return []metadata.Meta{
					{
						BlockMeta: tsdb.BlockMeta{
							MinTime: 0,
							MaxTime: (10 * time.Minute).Milliseconds(),
							Compaction: tsdb.BlockMetaCompaction{
								Sources: []ulid.ULID{block1, block2},
							},
						},
						Thanos: metadata.Thanos{
							Labels: map[string]string{
								mimir_tsdb.TenantIDExternalLabel: userID,
								ShardIDLabelName:                 "1_of_2",
							},
						},
					}, {
						BlockMeta: tsdb.BlockMeta{
							MinTime: 0,
							MaxTime: (10 * time.Minute).Milliseconds(),
							Compaction: tsdb.BlockMetaCompaction{
								Sources: []ulid.ULID{block1, block2},
							},
						},
						Thanos: metadata.Thanos{
							Labels: map[string]string{
								mimir_tsdb.TenantIDExternalLabel: userID,
								ShardIDLabelName:                 "2_of_2",
							},
						},
					}, {
						// Not compacted.
						BlockMeta: tsdb.BlockMeta{
							MinTime: blockRangeMillis,
							MaxTime: blockRangeMillis + time.Minute.Milliseconds(),
							Compaction: tsdb.BlockMetaCompaction{
								Sources: []ulid.ULID{block3},
							},
						},
						Thanos: metadata.Thanos{
							Labels: map[string]string{
								mimir_tsdb.TenantIDExternalLabel: userID,
							},
						},
					},
				}
			},
		},
		"smaller compaction ranges should take precedence over larger ones, and then re-iterate in subsequent " +
			"compactions of increasing ranges": {
			setup: func(t *testing.T, bkt objstore.Bucket) []metadata.Meta {
				// Two split blocks in the 1st compaction range.
				block1a := createTSDBBlock(t, bkt, userID, 1, blockRangeMillis, numSeries, externalLabels("1_of_2"))
				block1b := createTSDBBlock(t, bkt, userID, 1, blockRangeMillis, numSeries, externalLabels("2_of_2"))

				// Two non-split overlapping blocks in the 1st compaction range.
				block2 := createTSDBBlock(t, bkt, userID, blockRangeMillis, 2*blockRangeMillis, numSeries, externalLabels(""))
				block3 := createTSDBBlock(t, bkt, userID, blockRangeMillis, 2*blockRangeMillis, numSeries, externalLabels(""))

				// Two split adjacent blocks in the 2nd compaction range.
				block4a := createTSDBBlock(t, bkt, userID, 2*blockRangeMillis, 3*blockRangeMillis, numSeries, externalLabels("1_of_2"))
				block4b := createTSDBBlock(t, bkt, userID, 2*blockRangeMillis, 3*blockRangeMillis, numSeries, externalLabels("2_of_2"))
				block5a := createTSDBBlock(t, bkt, userID, 3*blockRangeMillis, 4*blockRangeMillis, numSeries, externalLabels("1_of_2"))
				block5b := createTSDBBlock(t, bkt, userID, 3*blockRangeMillis, 4*blockRangeMillis, numSeries, externalLabels("2_of_2"))

				// Two non-adjacent non-split blocks in the 1st compaction range.
				block6 := createTSDBBlock(t, bkt, userID, 4*blockRangeMillis, 5*blockRangeMillis, numSeries, externalLabels(""))
				block7 := createTSDBBlock(t, bkt, userID, 7*blockRangeMillis, 8*blockRangeMillis, numSeries, externalLabels(""))

				return []metadata.Meta{
					// The two overlapping blocks (block2, block3) have been merged and split in the 1st range,
					// and then compacted with block1 in 2nd range. Finally, they've been compacted with
					// block4 and block5 in the 3rd range compaction (total levels: 4).
					{
						BlockMeta: tsdb.BlockMeta{
							MinTime: 1,
							MaxTime: 4 * blockRangeMillis,
							Compaction: tsdb.BlockMetaCompaction{
								Sources: []ulid.ULID{block1a, block2, block3, block4a, block5a},
							},
						},
						Thanos: metadata.Thanos{
							Labels: map[string]string{
								mimir_tsdb.TenantIDExternalLabel: userID,
								ShardIDLabelName:                 "1_of_2",
							},
						},
					}, {
						BlockMeta: tsdb.BlockMeta{
							MinTime: 1,
							MaxTime: 4 * blockRangeMillis,
							Compaction: tsdb.BlockMetaCompaction{
								Sources: []ulid.ULID{block1b, block2, block3, block4b, block5b},
							},
						},
						Thanos: metadata.Thanos{
							Labels: map[string]string{
								mimir_tsdb.TenantIDExternalLabel: userID,
								ShardIDLabelName:                 "2_of_2",
							},
						},
					},
					// The two non-adjacent blocks block6 and block7 are split individually first and then merged
					// together in the 3rd range.
					{
						BlockMeta: tsdb.BlockMeta{
							MinTime: 4 * blockRangeMillis,
							MaxTime: 8 * blockRangeMillis,
							Compaction: tsdb.BlockMetaCompaction{
								Sources: []ulid.ULID{block6, block7},
							},
						},
						Thanos: metadata.Thanos{
							Labels: map[string]string{
								mimir_tsdb.TenantIDExternalLabel: userID,
								ShardIDLabelName:                 "1_of_2",
							},
						},
					}, {
						BlockMeta: tsdb.BlockMeta{
							MinTime: 4 * blockRangeMillis,
							MaxTime: 8 * blockRangeMillis,
							Compaction: tsdb.BlockMetaCompaction{
								Sources: []ulid.ULID{block6, block7},
							},
						},
						Thanos: metadata.Thanos{
							Labels: map[string]string{
								mimir_tsdb.TenantIDExternalLabel: userID,
								ShardIDLabelName:                 "2_of_2",
							},
						},
					},
				}
			},
		},
		"overlapping and non-overlapping blocks within the same range should be split and compacted together": {
			setup: func(t *testing.T, bkt objstore.Bucket) []metadata.Meta {
				// Overlapping.
				block1 := createTSDBBlock(t, bkt, userID, 0, (5 * time.Minute).Milliseconds(), numSeries, externalLabels(""))
				block2 := createTSDBBlock(t,
					bkt,
					userID,
					time.Minute.Milliseconds(),
					(7 * time.Minute).Milliseconds(),
					numSeries,
					externalLabels(""))

				// Not overlapping.
				block3 := createTSDBBlock(t,
					bkt,
					userID,
					time.Hour.Milliseconds(),
					(2 * time.Hour).Milliseconds(),
					numSeries,
					externalLabels(""))

				return []metadata.Meta{
					{
						BlockMeta: tsdb.BlockMeta{
							MinTime: 0,
							MaxTime: (2 * time.Hour).Milliseconds(),
							Compaction: tsdb.BlockMetaCompaction{
								Sources: []ulid.ULID{block1, block2, block3},
							},
						},
						Thanos: metadata.Thanos{
							Labels: map[string]string{
								mimir_tsdb.TenantIDExternalLabel: userID,
								ShardIDLabelName:                 "1_of_2",
							},
						},
					}, {
						BlockMeta: tsdb.BlockMeta{
							MinTime: 0,
							MaxTime: (2 * time.Hour).Milliseconds(),
							Compaction: tsdb.BlockMetaCompaction{
								Sources: []ulid.ULID{block1, block2, block3},
							},
						},
						Thanos: metadata.Thanos{
							Labels: map[string]string{
								mimir_tsdb.TenantIDExternalLabel: userID,
								ShardIDLabelName:                 "2_of_2",
							},
						},
					},
				}
			},
		},
		"should correctly handle empty blocks generated in the splitting stage": {
			setup: func(t *testing.T, bkt objstore.Bucket) []metadata.Meta {
				// Generate a block with only 1 series. This block will be split into 1 split block only,
				// because the source block only has 1 series.
				block1 := createTSDBBlock(t, bkt, userID, blockRangeMillis, 2*blockRangeMillis, 1, externalLabels(""))

				return []metadata.Meta{
					{
						BlockMeta: tsdb.BlockMeta{
							MinTime: (2 * blockRangeMillis) - 1, // Because there's only 1 sample with timestamp=maxT-1
							MaxTime: 2 * blockRangeMillis,
							Compaction: tsdb.BlockMetaCompaction{
								Sources: []ulid.ULID{block1},
							},
						},
						Thanos: metadata.Thanos{
							Labels: map[string]string{
								mimir_tsdb.TenantIDExternalLabel: userID,
								ShardIDLabelName:                 "1_of_2",
							},
						},
					},
				}
			},
		},
	}

	for testName, testData := range tests {
		t.Run(testName, func(t *testing.T) {
			// Create a temporary directory for compactor.
			workDir, err := ioutil.TempDir(os.TempDir(), "compactor")
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, os.RemoveAll(workDir))
			})

			// Create a temporary directory for local storage.
			storageDir, err := ioutil.TempDir(os.TempDir(), "storage")
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, os.RemoveAll(storageDir))
			})

			// Create a temporary directory for fetcher.
			fetcherDir, err := ioutil.TempDir(os.TempDir(), "fetcher")
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, os.RemoveAll(fetcherDir))
			})

			storageCfg := mimir_tsdb.BlocksStorageConfig{}
			flagext.DefaultValues(&storageCfg)
			storageCfg.Bucket.Backend = bucket.Filesystem
			storageCfg.Bucket.Filesystem.Directory = storageDir

			compactorCfg := prepareConfig()
			compactorCfg.DataDir = workDir
			compactorCfg.BlockRanges = compactionRanges
			compactorCfg.CompactionStrategy = CompactionStrategySplitMerge

			cfgProvider := newMockConfigProvider()
			cfgProvider.splitAndMergeShards[userID] = numShards

			logger := log.NewLogfmtLogger(os.Stdout)
			reg := prometheus.NewPedanticRegistry()
			ctx := context.Background()

			// Create TSDB blocks in the storage and get the expected blocks.
			bucketClient, err := bucket.NewClient(ctx, storageCfg.Bucket, "test", logger, nil)
			require.NoError(t, err)
			expected := testData.setup(t, bucketClient)

			c, err := NewMultitenantCompactor(compactorCfg, storageCfg, cfgProvider, logger, reg)
			require.NoError(t, err)
			require.NoError(t, services.StartAndAwaitRunning(context.Background(), c))
			t.Cleanup(func() {
				require.NoError(t, services.StopAndAwaitTerminated(context.Background(), c))
			})

			// Wait until the first compaction run completed.
			test.Poll(t, 10*time.Second, nil, func() interface{} {
				return testutil.GatherAndCompare(reg, strings.NewReader(`
					# HELP cortex_compactor_runs_completed_total Total number of compaction runs successfully completed.
					# TYPE cortex_compactor_runs_completed_total counter
					cortex_compactor_runs_completed_total 1
				`), "cortex_compactor_runs_completed_total")
			})

			// List back any (non deleted) block from the storage.
			userBucket := bucket.NewUserBucketClient(userID, bucketClient, nil)
			fetcher, err := block.NewMetaFetcher(logger,
				1,
				userBucket,
				fetcherDir,
				reg,
				[]block.MetadataFilter{block.NewIgnoreDeletionMarkFilter(logger,
					userBucket,
					0,
					block.FetcherConcurrency)},
				nil)
			require.NoError(t, err)
			metas, partials, err := fetcher.Fetch(ctx)
			require.NoError(t, err)
			require.Empty(t, partials)

			// Sort blocks by MinTime and labels so that we get a stable comparison.
			var actual []*metadata.Meta
			for _, m := range metas {
				actual = append(actual, m)
			}

			sort.Slice(actual, func(i, j int) bool {
				if actual[i].BlockMeta.MinTime != actual[j].BlockMeta.MinTime {
					return actual[i].BlockMeta.MinTime < actual[j].BlockMeta.MinTime
				}

				return labels.Compare(labels.FromMap(actual[i].Thanos.Labels), labels.FromMap(actual[j].Thanos.Labels)) < 0
			})

			// Compare actual blocks with the expected ones.
			require.Len(t, actual, len(expected))
			for i, e := range expected {
				assert.Equal(t, e.MinTime, actual[i].MinTime)
				assert.Equal(t, e.MaxTime, actual[i].MaxTime)
				assert.Equal(t, e.Compaction.Sources, actual[i].Compaction.Sources)
				assert.Equal(t, e.Thanos.Labels, actual[i].Thanos.Labels)
			}
		})
	}
}
