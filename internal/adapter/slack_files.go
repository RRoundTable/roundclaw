package adapter

import (
	"context"
	"fmt"
	"net/http"

	"github.com/slack-go/slack"
)

// attachFiles downloads a message's uploads to the agent's staging directory
// and returns the prompt with their container paths prepended, alongside the
// staged host paths for the turn row.
//
// A download failure fails the whole request rather than proceeding without the
// file, exactly as the Discord edge does: silently answering a question about a
// document nobody managed to deliver is worse than saying so.
func (s *Slack) attachFiles(ctx context.Context, agentID, text string, uploads []slack.File) (string, []string, error) {
	if len(uploads) == 0 {
		return text, nil, nil
	}
	if len(uploads) > MaxAttachments {
		return "", nil, fmt.Errorf("too many files: %d, limit is %d", len(uploads), MaxAttachments)
	}

	var files []Attachment
	var bodies []interface{ Close() error }
	defer func() {
		for _, b := range bodies {
			b.Close()
		}
	}()

	for _, u := range uploads {
		name := u.Name
		if name == "" {
			name = u.Title
		}
		if name == "" {
			name = u.ID
		}
		// Checked before downloading so an oversized file costs nothing.
		if int64(u.Size) > MaxAttachmentBytes {
			return "", nil, fmt.Errorf("%s is %.1fMB; the limit is %dMB",
				name, float64(u.Size)/(1<<20), MaxAttachmentBytes>>20)
		}

		// URLPrivateDownload serves the bytes; URLPrivate serves Slack's viewer
		// page. Both need the bot token — an unauthenticated fetch of either
		// returns a sign-in page with status 200, which would be staged as the
		// file and handed to the agent as though it were the document.
		if u.URLPrivateDownload == "" {
			return "", nil, fmt.Errorf("slack gave no download url for %s", name)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.URLPrivateDownload, nil)
		if err != nil {
			return "", nil, fmt.Errorf("fetch %s: %w", name, err)
		}
		req.Header.Set("Authorization", "Bearer "+s.botToken)

		resp, err := attachmentClient.Do(req)
		if err != nil {
			return "", nil, fmt.Errorf("fetch %s: %w", name, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return "", nil, fmt.Errorf("fetch %s: slack returned %s", name, resp.Status)
		}
		bodies = append(bodies, resp.Body)
		files = append(files, Attachment{Name: name, Body: resp.Body, Size: int64(u.Size)})
	}

	staged, err := s.disp.StageAttachments(agentID, files)
	if err != nil {
		return "", nil, err
	}
	s.log.Info("staged slack attachments", "agent", agentID, "files", len(staged.HostPaths))
	return PromptWithAttachments(text, staged.ContainerPaths), staged.HostPaths, nil
}
