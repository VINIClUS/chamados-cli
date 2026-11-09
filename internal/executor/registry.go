package executor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

func packagePath(environment, id, file string) (string, error) {
	if (environment != "staging" && environment != "production") || !operations.BackupVersion.MatchString(id) {
		return "", fmt.Errorf("invalid backup package")
	}
	return "packages/generic/chamados-backup-" + environment + "/" + id + "/" + file, nil
}

func (c *JobClient) Manifest(ctx context.Context, environment, id string) (operations.Manifest, error) {
	var m operations.Manifest
	path, err := packagePath(environment, id, "manifest.json")
	if err != nil {
		return m, err
	}
	err = c.JSON(ctx, "GET", path, nil, &m)
	if err != nil {
		return m, err
	}
	if !m.Valid(environment) {
		return m, fmt.Errorf("incomplete backup manifest")
	}
	return m, nil
}

func (c *JobClient) Publish(ctx context.Context, environment, id, dir string) error {
	var m operations.Manifest
	if err := operations.ReadJSON(filepath.Join(dir, "manifest.json"), &m); err != nil {
		return err
	}
	if !m.MetadataValid() || m.Environment != environment {
		return fmt.Errorf("invalid published metadata")
	}
	sum, err := operations.Digest(filepath.Join(dir, "backup.tar.gz"))
	if err != nil {
		return err
	}
	if sum != m.SHA256 {
		return fmt.Errorf("refusing corrupt backup")
	}
	marker, err := packagePath(environment, id, "manifest.json")
	if err != nil {
		return err
	}
	err = c.JSON(ctx, "GET", marker, nil, nil)
	var he *HTTPError
	if err == nil {
		return fmt.Errorf("backup ID already committed")
	}
	if !errors.As(err, &he) || he.Status != 404 {
		return err
	}
	archive, _ := packagePath(environment, id, "backup.tar.gz")
	if err = c.Upload(ctx, archive, filepath.Join(dir, "backup.tar.gz")); err != nil {
		return err
	}
	return c.Upload(ctx, marker, filepath.Join(dir, "manifest.json"))
}

func (c *JobClient) DownloadBackup(ctx context.Context, environment, id, dir string) error {
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	for _, name := range []string{"manifest.json", "backup.tar.gz"} {
		path, err := packagePath(environment, id, name)
		if err == nil {
			err = c.Download(ctx, path, filepath.Join(dir, name))
		}
		if err != nil {
			_ = os.RemoveAll(dir)
			return err
		}
	}
	return nil
}

type registryPackage struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	Type      string    `json:"package_type"`
	CreatedAt time.Time `json:"created_at"`
}

func (c *JobClient) Cleanup(ctx context.Context, environment string) error {
	name := "chamados-backup-" + environment
	var packages []registryPackage
	for page := 1; ; page++ {
		var batch []registryPackage
		query := url.Values{
			"package_type": {"generic"},
			"package_name": {name},
			"per_page":     {"100"},
			"page":         {fmt.Sprint(page)},
			"order_by":     {"created_at"},
			"sort":         {"desc"},
		}
		if err := c.JSON(ctx, "GET", "packages?"+query.Encode(), nil, &batch); err != nil {
			return err
		}
		for _, p := range batch {
			if p.Name == name && p.Type == "generic" && operations.BackupVersion.MatchString(p.Version) {
				packages = append(packages, p)
			}
		}
		if len(batch) < 100 {
			break
		}
	}
	var keep int64
	for _, p := range packages {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		tmp, err := os.MkdirTemp("", "chamados-retention-*")
		if err != nil {
			return err
		}
		err = c.DownloadBackup(ctx, environment, p.Version, filepath.Join(tmp, "package"))
		var m operations.Manifest
		if err == nil {
			err = operations.ReadJSON(filepath.Join(tmp, "package/manifest.json"), &m)
		}
		if err == nil {
			_, err = operations.Verify(
				filepath.Join(tmp, "package/backup.tar.gz"),
				filepath.Join(tmp, "package/manifest.json"),
				environment,
				m.Major(),
				filepath.Join(tmp, "verified"),
			)
		}
		_ = os.RemoveAll(tmp)
		if err == nil {
			keep = p.ID
			break
		}
	}
	if keep == 0 && len(packages) > 0 {
		return fmt.Errorf("cleanup refused: no complete valid backup")
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -30)
	for _, p := range packages {
		if p.ID != keep && p.CreatedAt.Before(cutoff) {
			if err := c.JSON(ctx, "DELETE", fmt.Sprintf("packages/%d", p.ID), nil, nil); err != nil {
				return err
			}
		}
	}
	return nil
}
