package adapter

import (
	"encoding/json"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
)

// readSubmission parses a request in either shape.
//
// JSON stays the default; multipart is accepted so a file can be sent with the
// request in one call:
//
//	curl -F text="review this" -F file=@report.pdf .../requests
//
// Two calls (upload, then reference) would be tidier REST but would leave
// orphaned uploads whenever the second call never arrives.
//
// The returned cleanup releases multipart's temporary files and must be called.
func readSubmission(w http.ResponseWriter, r *http.Request) (submitBody, []Attachment, func(), error) {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if !strings.HasPrefix(mediaType, "multipart/") {
		var body submitBody
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			return body, nil, nil, fmt.Errorf("malformed JSON body: %w", err)
		}
		return body, nil, nil, nil
	}

	// Bounded so a large upload is streamed to disk rather than held in memory.
	const inMemory = 8 << 20
	if err := r.ParseMultipartForm(inMemory); err != nil {
		return submitBody{}, nil, nil, fmt.Errorf("malformed multipart body: %w", err)
	}
	form := r.MultipartForm
	cleanup := func() {
		if form != nil {
			form.RemoveAll()
		}
	}

	body := submitBody{
		Text:        r.FormValue("text"),
		CallbackURL: r.FormValue("callback_url"),
		Steer:       r.FormValue("steer") == "true",
	}

	var files []Attachment
	var opened []*multipart.FileHeader
	for _, headers := range form.File {
		opened = append(opened, headers...)
	}
	if len(opened) > MaxAttachments {
		cleanup()
		return body, nil, nil, fmt.Errorf("too many files: %d, limit is %d", len(opened), MaxAttachments)
	}
	for _, fh := range opened {
		f, err := fh.Open()
		if err != nil {
			cleanup()
			return body, nil, nil, fmt.Errorf("read upload %s: %w", fh.Filename, err)
		}
		// Closed by cleanup via RemoveAll; holding them open until the copy is
		// done is the point.
		files = append(files, Attachment{Name: fh.Filename, Body: f, Size: fh.Size})
	}
	return body, files, cleanup, nil
}
