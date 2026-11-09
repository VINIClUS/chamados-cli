package executor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

func (c *JobClient) PublishRelease(ctx context.Context, ref string) error {
	if !operations.ReleaseTag.MatchString(ref) {
		return fmt.Errorf("invalid system release tag")
	}
	path := "releases/" + url.PathEscape(ref)
	err := c.JSON(ctx, "GET", path, nil, nil)
	if err == nil {
		return nil
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 404 {
		return err
	}
	if _, err = capture(ctx, "git", "fetch", "--quiet", "--tags", "origin"); err != nil {
		return err
	}
	previous, _ := capture(ctx, "git", "describe", "--tags", "--abbrev=0", "--match", "v[0-9]*", ref+"^")
	notes := "Primeira versão publicada pela pipeline."
	if previous != "" {
		body, logErr := capture(
			ctx,
			"git",
			"log",
			"--merges",
			"--first-parent",
			"--format=%b",
			previous+".."+ref,
		)
		if logErr != nil {
			return logErr
		}
		var titles []string
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "See merge request ") {
				titles = append(titles, "- "+line)
			}
		}
		changes := strings.Join(titles, "\n")
		if changes == "" {
			changes = "Nenhum merge desde a versão anterior."
		}
		notes = "Mudanças desde " + previous + ":\n\n" + changes
	}
	err = c.JSON(
		ctx,
		"POST",
		"releases",
		map[string]string{"tag_name": ref, "name": ref, "description": notes},
		nil,
	)
	if errors.As(err, &he) && he.Status == 409 {
		return nil
	}
	return err
}
