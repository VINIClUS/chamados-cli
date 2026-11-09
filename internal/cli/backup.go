package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/VINIClUS/chamados-cli/internal/operations"
	"github.com/spf13/cobra"
)

type Member = operations.Member

type Manifest = operations.Manifest

type Backup struct {
	BackupID string `json:"backup_id"`
	Manifest
}

var fullSHA = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)

var errIncompleteManifest = errors.New("backup manifest is incomplete or incompatible")

func backupPath(env, id, file string) string {
	return "/packages/generic/chamados-backup-" + env + "/" + url.PathEscape(id) + "/" + file
}

func (c *apiClient) manifest(ctx context.Context, env, id string) (Manifest, error) {
	var manifest Manifest
	if err := c.json(ctx, "GET", backupPath(env, id, "manifest.json"), nil, &manifest); err != nil {
		if errors.Is(err, errInvalidJSON) {
			return manifest, errIncompleteManifest
		}
		return manifest, err
	}
	if !manifest.Valid(env) {
		return manifest, errIncompleteManifest
	}
	return manifest, nil
}

func (a *app) addBackupCommands(parent *cobra.Command) {
	var listEnv string
	list := &cobra.Command{
		Use:   "list [environment]",
		Short: "List backups with complete published manifests",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				listEnv = args[0]
			}
			if err := validateEnvironment(listEnv); err != nil {
				return err
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			backups := []Backup{}
			for page := 1; ; page++ {
				var packages []struct {
					Name    string `json:"name"`
					Version string `json:"version"`
				}
				query := url.Values{
					"package_type": {"generic"},
					"package_name": {"chamados-backup-" + listEnv},
					"per_page":     {"100"},
					"page":         {fmt.Sprint(page)},
					"order_by":     {"created_at"},
					"sort":         {"desc"},
				}
				if err := c.json(
					cmd.Context(),
					"GET",
					"/packages?"+query.Encode(),
					nil,
					&packages,
				); err != nil {
					return err
				}
				for _, pkg := range packages {
					if pkg.Name != "chamados-backup-"+listEnv ||
						!backupVersion.MatchString(pkg.Version) {
						continue
					}
					manifest, err := c.manifest(cmd.Context(), listEnv, pkg.Version)
					if err != nil {
						var ae *apiError
						if errors.Is(err, errIncompleteManifest) ||
							(errors.As(err, &ae) && ae.status == 404) {
							continue
						}
						return err
					}
					backups = append(backups, Backup{pkg.Version, manifest})
				}
				if len(packages) < 100 {
					break
				}
			}
			return a.output(backups, c.token)
		},
	}
	environmentFlags(list, &listEnv)
	var downloadEnv, dest, backupID string
	download := &cobra.Command{
		Use:   "download <environment> --backup ID",
		Short: "Download and verify a backup without overwriting files",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && backupVersion.MatchString(args[0]) {
				if backupID != "" {
					return fmt.Errorf("specify backup ID only once")
				}
				backupID = args[0]
			} else if len(args) == 1 {
				downloadEnv = args[0]
			}
			if err := validateEnvironment(downloadEnv); err != nil {
				return err
			}
			id := backupID
			if !backupVersion.MatchString(id) {
				return fmt.Errorf("download requires --backup ID (YYYYMMDDHHMMSS.pipeline_iid)")
			}
			if dest == "" {
				dest = "backup-" + downloadEnv + "-" + id + ".tar.gz"
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			manifest, err := c.manifest(cmd.Context(), downloadEnv, id)
			if err != nil {
				return err
			}
			if err := c.download(
				cmd.Context(),
				backupPath(downloadEnv, id, "backup.tar.gz"),
				dest,
				manifest.SHA256,
			); err != nil {
				return err
			}
			return a.output(
				map[string]any{"backup_id": id, "path": dest, "sha256": manifest.SHA256},
				c.token,
			)
		},
	}
	environmentFlags(download, &downloadEnv)
	download.Flags().StringVarP(&dest, "output", "o", "", "Destination file (must not exist)")
	download.Flags().StringVar(&backupID, "backup", "", "Backup ID (YYYYMMDDHHMMSS.pipeline_iid)")
	parent.AddCommand(list, download)
}

func environmentFlags(cmd *cobra.Command, dest *string) {
	cmd.Flags().StringVar(dest, "env", "staging", "Environment: staging or production")
	cmd.Flags().StringVar(dest, "environment", "staging", "Environment: staging or production")
}

func validateEnvironment(env string) error {
	if env != "staging" && env != "production" {
		return fmt.Errorf("environment must be staging or production")
	}
	return nil
}

func (c *apiClient) download(ctx context.Context, path, dest, checksum string) error {
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("destination already exists")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("cannot inspect destination")
	}
	temp, err := os.CreateTemp(filepath.Dir(dest), ".chamadosctl-download-*")
	if err != nil {
		return fmt.Errorf("cannot create download file")
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	resp, err := c.get(ctx, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(temp, hash), resp.Body); err != nil {
		return fmt.Errorf("backup download interrupted")
	}
	if !strings.EqualFold(fmt.Sprintf("%x", hash.Sum(nil)), checksum) {
		return fmt.Errorf("backup SHA256 checksum mismatch")
	}
	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("cannot read verified download")
	}
	output, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("cannot create destination (must not exist)")
	}
	_, copyErr := io.Copy(output, temp)
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(dest)
		return fmt.Errorf("cannot save verified backup")
	}
	return nil
}
