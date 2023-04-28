// SPDX-License-Identifier: AGPL-3.0-only

package querier

import (
	"errors"
	"fmt"
	"io"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb/chunkenc"

	"github.com/grafana/mimir/pkg/ingester/client"
	"github.com/grafana/mimir/pkg/storage/chunk"
	"github.com/grafana/mimir/pkg/storage/series"
	"github.com/grafana/mimir/pkg/util/chunkcompat"
)

type streamingChunkSeries struct {
	labels labels.Labels

	chunkIteratorFunc chunkIteratorFunc
	mint, maxt        int64

	ingesters []struct {
		streamer    SeriesStreamer
		seriesIndex uint64
	}
}

func (s *streamingChunkSeries) Labels() labels.Labels {
	return s.labels
}

func (s *streamingChunkSeries) Iterator(_ chunkenc.Iterator) chunkenc.Iterator {
	var chunks []chunk.Chunk // TODO: guess a size for this?

	for _, ingester := range s.ingesters {
		c, err := ingester.streamer.GetChunks(ingester.seriesIndex)

		if err != nil {
			return series.NewErrIterator(err)
		}

		// TODO: deduplicate chunks (use similar function to accumulateChunks)
		chunks = append(chunks, c...)
	}

	return s.chunkIteratorFunc(chunks, model.Time(s.mint), model.Time(s.maxt))
}

type SeriesStreamer struct {
	client       client.Ingester_QueryStreamClient
	buffer       chan streamedIngesterSeries
	errChan      chan error
	SeriesLabels []labels.Labels
}

func NewSeriesStreamer(client client.Ingester_QueryStreamClient, seriesLabels []labels.Labels) *SeriesStreamer {
	return &SeriesStreamer{
		client:       client,
		SeriesLabels: seriesLabels,
	}
}

type streamedIngesterSeries struct {
	chunks      []chunk.Chunk
	seriesIndex uint64
}

// Close cleans up any resources associated with this SeriesStreamer.
// This method should only be called if StartBuffering is never called.
func (s *SeriesStreamer) Close() error {
	return s.client.CloseSend()
}

// StartBuffering begins streaming series' chunks from the ingester associated with
// this SeriesStreamer. Once all series have been consumed with GetChunks, all resources
// associated with this SeriesStreamer are cleaned up.
// If an error occurs while streaming, the next call to GetChunks will return an error.
func (s *SeriesStreamer) StartBuffering() {
	// TODO: need a way to cancel this method and close the stream cleanly
	// Or can we rely on the gRPC call's context being cancelled?

	s.buffer = make(chan streamedIngesterSeries, 10) // TODO: what is a sensible buffer size?
	s.errChan = make(chan error, 1)                  // Buffered channel so that if an error occurs, this goroutine doesn't wait for it to be handled and immediately begins cleaning up.

	go func() {
		defer s.client.CloseSend()
		defer close(s.buffer)
		defer close(s.errChan)

		nextSeriesIndex := uint64(0)

		for {
			msg, err := s.client.Recv()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					s.errChan <- err
				}

				return
			}

			for _, series := range msg.SeriesChunks {
				chunks, err := chunkcompat.FromChunks(s.SeriesLabels[nextSeriesIndex], series.Chunks)
				if err != nil {
					s.errChan <- err
					return
				}

				s.buffer <- streamedIngesterSeries{
					chunks:      chunks,
					seriesIndex: nextSeriesIndex,
				}

				nextSeriesIndex++
			}
		}
	}()
}

// GetChunks returns the chunks for the series with index seriesIndex.
// This method must be called with monotonically increasing values of seriesIndex.
func (s *SeriesStreamer) GetChunks(seriesIndex uint64) ([]chunk.Chunk, error) {
	select {
	case err := <-s.errChan:
		return nil, fmt.Errorf("attempted to read series at index %v from stream, but the stream has failed: %w", seriesIndex, err)

	case series, open := <-s.buffer:
		if !open {
			return nil, fmt.Errorf("attempted to read series at index %v from stream, but the stream has already been closed", seriesIndex)
		}

		if series.seriesIndex != seriesIndex {
			return nil, fmt.Errorf("attempted to read series at index %v from stream, but the stream has series with index %v", seriesIndex, series.seriesIndex)
		}

		return series.chunks, nil
	}
}