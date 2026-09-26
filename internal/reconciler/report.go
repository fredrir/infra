package reconciler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/objectstore"
	"github.com/fredrir/infra/internal/reconcile"
	"github.com/klauspost/compress/zstd"
)

const (
	OutcomeSkipped = "skipped"
	summaryLimit   = 900
)

type Run struct {
	Kind         string                  `json:"kind"`
	Revision     string                  `json:"revision,omitempty"`
	Started      time.Time               `json:"started_at"`
	Finished     time.Time               `json:"finished_at"`
	Stage        string                  `json:"stage"`
	Outcome      string                  `json:"outcome"`
	Error        string                  `json:"error,omitempty"`
	Verification *reconcile.Verification `json:"verification,omitempty"`
}

func (r Run) ID() string {
	revision := "unresolved"
	if revisionPattern.MatchString(r.Revision) {
		revision = r.Revision[:12]
	}
	return r.Started.UTC().Format("20060102T150405Z") + "-" + r.Kind + "-" + revision
}

func (r Run) failure() error {
	switch r.Outcome {
	case reconcile.OutcomeMatches, OutcomeSkipped:
		return nil
	case reconcile.OutcomeDiffers:
		items := make([]string, 0, len(r.Verification.Differences))
		for _, difference := range r.Verification.Differences {
			item := difference.System + ": "
			if difference.Host != "" {
				item += difference.Host + ": "
			}
			items = append(items, item+difference.Item)
		}
		return errors.New(truncate(fmt.Sprintf("%d differences: %s", len(items), strings.Join(items, "; "))))
	}
	if r.Verification != nil && len(r.Verification.Errors) > 0 {
		return errors.New(truncate(r.Stage + ": " + strings.Join(r.Verification.Errors, "; ")))
	}
	return errors.New(truncate(r.Stage + ": " + r.Error))
}

func truncate(text string) string {
	if len(text) <= summaryLimit {
		return text
	}
	return strings.ToValidUTF8(text[:summaryLimit], "") + "…"
}

type Store struct {
	Client objectstore.Client
	Bucket string
	Prefix string
}

func (s Store) upload(ctx context.Context, run Run, log []byte) error {
	report, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed)
	if err != nil {
		return err
	}
	if _, err := encoder.Write(log); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	directory := s.Prefix + "/runs/" + run.ID() + "/"
	for _, object := range []struct {
		name, contentType string
		body              []byte
	}{{"report.json", "application/json", append(report, '\n')}, {"log.txt.zst", "application/zstd", compressed.Bytes()}} {
		headers := http.Header{"Content-Type": {object.contentType}, "X-Amz-Server-Side-Encryption": {"AES256"}}
		response, err := s.Client.RequestHeaders(ctx, http.MethodPut, s.Bucket, directory+object.name, bytes.NewReader(object.body), headers)
		if err != nil {
			return fmt.Errorf("upload %s: %w", directory+object.name, err)
		}
		_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		if err = errors.Join(err, response.Body.Close()); err != nil {
			return fmt.Errorf("upload %s: %w", directory+object.name, err)
		}
	}
	return nil
}
