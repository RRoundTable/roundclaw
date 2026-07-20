package adapter

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/bwmarrin/discordgo"
)

// attachmentClient fetches uploads from Discord's CDN. Separate from the
// default client so a slow download cannot hold a connection open forever.
var attachmentClient = &http.Client{Timeout: 60 * time.Second}

// attachFiles downloads a message's or command's attachments into the agent's
// workspace and returns the prompt with their paths prepended.
//
// A download failure fails the whole request rather than proceeding without the
// file. Silently answering a question about a document nobody managed to
// deliver is worse than saying so.
func (d *Discord) attachFiles(ctx context.Context, agentID, text string, attachments []*discordgo.MessageAttachment) (string, error) {
	if len(attachments) == 0 {
		return text, nil
	}
	if len(attachments) > MaxAttachments {
		return "", fmt.Errorf("too many files: %d, limit is %d", len(attachments), MaxAttachments)
	}

	var files []Attachment
	var bodies []interface{ Close() error }
	defer func() {
		for _, b := range bodies {
			b.Close()
		}
	}()

	for _, a := range attachments {
		// Checked before downloading so an oversized file costs nothing.
		if int64(a.Size) > MaxAttachmentBytes {
			return "", fmt.Errorf("%s is %.1fMB; the limit is %dMB",
				a.Filename, float64(a.Size)/(1<<20), MaxAttachmentBytes>>20)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
		if err != nil {
			return "", fmt.Errorf("fetch %s: %w", a.Filename, err)
		}
		resp, err := attachmentClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("fetch %s: %w", a.Filename, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return "", fmt.Errorf("fetch %s: discord returned %s", a.Filename, resp.Status)
		}
		bodies = append(bodies, resp.Body)
		files = append(files, Attachment{Name: a.Filename, Body: resp.Body, Size: int64(a.Size)})
	}

	paths, err := d.disp.SaveAttachments(agentID, files)
	if err != nil {
		return "", err
	}
	d.log.Info("saved attachments", "agent", agentID, "files", len(paths))
	return PromptWithAttachments(text, paths), nil
}

// discordAttachments pulls resolved attachments out of a slash command. The
// option carries an ID; the file itself arrives in the resolved data.
func discordAttachments(data discordgo.ApplicationCommandInteractionData) []*discordgo.MessageAttachment {
	if data.Resolved == nil || len(data.Resolved.Attachments) == 0 {
		return nil
	}
	var out []*discordgo.MessageAttachment
	for _, opt := range data.Options {
		if opt.Type != discordgo.ApplicationCommandOptionAttachment {
			continue
		}
		id, ok := opt.Value.(string)
		if !ok {
			continue
		}
		if a := data.Resolved.Attachments[id]; a != nil {
			out = append(out, a)
		}
	}
	return out
}
