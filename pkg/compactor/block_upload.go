// SPDX-License-Identifier: AGPL-3.0-only

package compactor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/go-kit/log/level"
	"github.com/gorilla/mux"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
	"github.com/thanos-io/thanos/pkg/block/metadata"
	"github.com/thanos-io/thanos/pkg/objstore"

	"github.com/grafana/dskit/tenant"
	"github.com/grafana/regexp"

	"github.com/grafana/mimir/pkg/storage/bucket"
	mimir_tsdb "github.com/grafana/mimir/pkg/storage/tsdb"
)

// HandleBlockUpload handles requests for starting or completing block uploads.
func (c *MultitenantCompactor) HandleBlockUpload(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	blockID := vars["block"]
	if blockID == "" {
		http.Error(w, "missing block ID", http.StatusBadRequest)
		return
	}
	bULID, err := ulid.Parse(blockID)
	if err != nil {
		http.Error(w, "invalid block ID", http.StatusBadRequest)
		return
	}
	tenantID, ctx, err := tenant.ExtractTenantIDFromHTTPRequest(r)
	if err != nil {
		http.Error(w, "invalid tenant ID", http.StatusBadRequest)
		return
	}

	if r.URL.Query().Get("uploadComplete") == "true" {
		c.completeBlockUpload(ctx, w, r, tenantID, bULID)
	} else {
		c.createBlockUpload(ctx, w, r, tenantID, bULID)
	}
}

func (c *MultitenantCompactor) createBlockUpload(ctx context.Context, w http.ResponseWriter, r *http.Request,
	tenantID string, blockID ulid.ULID) {
	level.Debug(c.logger).Log("msg", "starting block upload", "user", tenantID, "block", blockID)

	bkt := bucket.NewUserBucketClient(string(tenantID), c.bucketClient, c.cfgProvider)

	exists, err := bkt.Exists(ctx, path.Join(blockID.String(), "meta.json"))
	if err != nil {
		level.Error(c.logger).Log("msg", "failed to check existence of meta.json in object storage",
			"user", tenantID, "block", blockID, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if exists {
		level.Debug(c.logger).Log("msg", "complete block already exists in object storage", "user", tenantID,
			"block", blockID)
		http.Error(w, "block already exists in object storage", http.StatusConflict)
		return
	}

	dec := json.NewDecoder(r.Body)
	var meta metadata.Meta
	if err := dec.Decode(&meta); err != nil {
		http.Error(w, "malformed request body", http.StatusBadRequest)
		return
	}

	if err := c.sanitizeMeta(tenantID, blockID, &meta); err != nil {
		var eBadReq errBadRequest
		if errors.As(err, &eBadReq) {
			level.Warn(c.logger).Log("msg", eBadReq.message, "user", tenantID,
				"block", blockID)
			http.Error(w, eBadReq.message, http.StatusBadRequest)
			return
		}

		level.Error(c.logger).Log("msg", "failed to sanitize meta.json", "user", tenantID,
			"block", blockID, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if err := c.uploadMeta(ctx, w, meta, blockID, tenantID, "meta.json.temp", bkt); err != nil {
		level.Error(c.logger).Log("msg", "failed to upload meta.json", "user", tenantID,
			"block", blockID, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// UploadBlockFile handles requests for uploading block files.
func (c *MultitenantCompactor) UploadBlockFile(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	blockID := vars["block"]
	if blockID == "" {
		http.Error(w, "missing block ID", http.StatusBadRequest)
		return
	}
	_, err := ulid.Parse(blockID)
	if err != nil {
		http.Error(w, "invalid block ID", http.StatusBadRequest)
		return
	}
	pth := r.URL.Query().Get("path")
	if pth == "" {
		http.Error(w, "missing or invalid file path", http.StatusBadRequest)
		return
	}

	tenantID, ctx, err := tenant.ExtractTenantIDFromHTTPRequest(r)
	if err != nil {
		http.Error(w, "invalid tenant ID", http.StatusBadRequest)
		return
	}

	if path.Base(pth) == "meta.json" {
		http.Error(w, "meta.json is not allowed", http.StatusBadRequest)
		return
	}

	rePath := regexp.MustCompile(`^(index|chunks/\d{6})$`)
	if !rePath.MatchString(pth) {
		http.Error(w, fmt.Sprintf("invalid path: %q", pth), http.StatusBadRequest)
		return
	}

	if r.Body == nil || r.ContentLength == 0 {
		http.Error(w, "file cannot be empty", http.StatusBadRequest)
		return
	}

	bkt := bucket.NewUserBucketClient(string(tenantID), c.bucketClient, c.cfgProvider)

	metaPath := path.Join(blockID, "meta.json.temp")
	exists, err := bkt.Exists(ctx, metaPath)
	if err != nil {
		level.Error(c.logger).Log("msg", "failed to check existence of meta.json.temp in object storage",
			"user", tenantID, "block", blockID, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !exists {
		http.Error(w, fmt.Sprintf("upload of block %s not started yet", blockID), http.StatusBadRequest)
		return
	}

	rdr, err := bkt.Get(ctx, metaPath)
	if err != nil {
		level.Error(c.logger).Log("msg", "failed to download meta.json.temp from object storage",
			"user", tenantID, "block", blockID, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	dec := json.NewDecoder(rdr)
	var meta metadata.Meta
	if err := dec.Decode(&meta); err != nil {
		level.Error(c.logger).Log("msg", "failed to decode meta.json.temp",
			"user", tenantID, "block", blockID, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// TODO: Verify that upload path and length correspond to file index

	dst := path.Join(blockID, pth)

	level.Debug(c.logger).Log("msg", "uploading block file to bucket", "user", tenantID,
		"destination", dst, "size", r.ContentLength)
	reader := bodyReader{
		r: r,
	}
	if err := bkt.Upload(ctx, dst, reader); err != nil {
		level.Error(c.logger).Log("msg", "failed uploading block file to bucket",
			"user", tenantID, "destination", dst, "err", err)
		http.Error(w, "failed uploading block file to bucket", http.StatusBadGateway)
		return
	}

	level.Debug(c.logger).Log("msg", "finished uploading block file to bucket",
		"user", tenantID, "block", blockID, "path", pth)

	w.WriteHeader(http.StatusOK)
}

func (c *MultitenantCompactor) completeBlockUpload(ctx context.Context, w http.ResponseWriter, r *http.Request,
	tenantID string, blockID ulid.ULID) {
	level.Debug(c.logger).Log("msg", "received request to complete block upload", "user", tenantID,
		"block", blockID, "content_length", r.ContentLength)

	bkt := bucket.NewUserBucketClient(tenantID, c.bucketClient, c.cfgProvider)

	rdr, err := bkt.Get(ctx, path.Join(blockID.String(), "meta.json.temp"))
	if err != nil {
		level.Error(c.logger).Log("msg", "failed to download meta.json.temp from object storage",
			"user", tenantID, "block", blockID, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	dec := json.NewDecoder(rdr)
	var meta metadata.Meta
	if err := dec.Decode(&meta); err != nil {
		level.Error(c.logger).Log("msg", "failed to decode meta.json",
			"user", tenantID, "block", blockID.String(), "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	level.Debug(c.logger).Log("msg", "completing block upload", "user",
		tenantID, "block", blockID, "files", len(meta.Thanos.Files))

	// Upload meta.json so block is considered complete
	if err := c.uploadMeta(ctx, w, meta, blockID, tenantID, "meta.json", bkt); err != nil {
		level.Error(c.logger).Log("msg", "failed to upload meta.json", "user", tenantID,
			"block", blockID, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if err := bkt.Delete(ctx, path.Join(blockID.String(), "meta.json.temp")); err != nil {
		level.Error(c.logger).Log("msg", "failed to delete meta.json.temp from block in object storage",
			"user", tenantID, "block", blockID, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	level.Debug(c.logger).Log("msg", "successfully completed block upload")

	w.WriteHeader(http.StatusOK)
}

type errBadRequest struct {
	message string
}

func (e errBadRequest) Error() string {
	return e.message
}

func (c *MultitenantCompactor) sanitizeMeta(tenantID string, blockID ulid.ULID, meta *metadata.Meta) error {
	if meta.Thanos.Labels == nil {
		meta.Thanos.Labels = map[string]string{}
	}

	meta.ULID = blockID
	meta.Thanos.Labels[mimir_tsdb.TenantIDExternalLabel] = tenantID

	var rejLbls []string
	for l, v := range meta.Thanos.Labels {
		switch l {
		// Preserve these labels
		case mimir_tsdb.TenantIDExternalLabel, mimir_tsdb.CompactorShardIDExternalLabel:
		// Remove unused labels
		case mimir_tsdb.IngesterIDExternalLabel, mimir_tsdb.DeprecatedShardIDExternalLabel:
			level.Debug(c.logger).Log("msg", "removing unused external label from meta.json",
				"block", blockID.String(), "user", tenantID, "label", l, "value", v)
			delete(meta.Thanos.Labels, l)
		default:
			rejLbls = append(rejLbls, l)
		}
	}

	if len(rejLbls) > 0 {
		level.Warn(c.logger).Log("msg", "rejecting unsupported external label(s) in meta.json",
			"block", blockID.String(), "user", tenantID, "labels", strings.Join(rejLbls, ","))
		return errBadRequest{message: fmt.Sprintf("unsupported external label(s): %s", strings.Join(rejLbls, ","))}
	}

	// Mark block source
	meta.Thanos.Source = "upload"

	return nil
}

func (c *MultitenantCompactor) uploadMeta(ctx context.Context, w http.ResponseWriter, meta metadata.Meta,
	blockID ulid.ULID, tenantID, name string, bkt objstore.Bucket) error {
	dst := path.Join(blockID.String(), name)
	level.Debug(c.logger).Log("msg", fmt.Sprintf("uploading %s to bucket", name), "dst", dst)
	buf := bytes.NewBuffer(nil)
	enc := json.NewEncoder(buf)
	if err := enc.Encode(meta); err != nil {
		return errors.Wrap(err, "failed to encode block metadata")
	}
	if err := bkt.Upload(ctx, dst, buf); err != nil {
		return errors.Wrapf(err, "failed uploading %s to bucket", name)
	}

	return nil
}

type bodyReader struct {
	r *http.Request
}

// ObjectSize implements thanos.ObjectSizer.
func (r bodyReader) ObjectSize() (int64, error) {
	return r.r.ContentLength, nil
}

// Read implements io.Reader.
func (r bodyReader) Read(b []byte) (int, error) {
	return r.r.Body.Read(b)
}