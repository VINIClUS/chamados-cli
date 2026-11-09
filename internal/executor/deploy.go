package executor

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

func readEnv(file string) (map[string]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(io.LimitReader(f, 1<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[strings.TrimSpace(key)] = strings.Trim(value, `"'`)
		}
	}
	return values, scanner.Err()
}

func randomSecret(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (e *execution) ensureEnv() error {
	file := filepath.Join(e.options.Dir, ".env")
	if _, err := os.Stat(file); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	public := e.options.PublicURL
	if public == "" {
		public = "https://stg-chamados.portosoftware.com.br"
		if e.env.Request.Environment == "production" {
			public = "https://chamados.portosoftware.com.br"
		}
	}
	u, err := url.Parse(public)
	if err != nil || u.Hostname() == "" || strings.ContainsAny(public, "\r\n\x00") {
		return fmt.Errorf("invalid public URL")
	}
	database, err := randomSecret(24)
	if err != nil {
		return err
	}
	jwt, err := randomSecret(48)
	if err != nil {
		return err
	}
	admin, err := randomSecret(24)
	if err != nil {
		return err
	}
	name := "Administrador do Staging"
	if e.env.Request.Environment == "production" {
		name = "Administrador do Sistema"
	}
	content := fmt.Sprintf(
		"POSTGRES_USER=chamados\nPOSTGRES_PASSWORD=%s\nPOSTGRES_DB=chamados\nJWT_SECRET=%s\nJWT_ACCESS_TOKEN_MINUTES=30\nCOOKIE_SECURE=true\nCOOKIE_SAME_SITE=Strict\nFRONTEND_URL=%s\nMAIL_ENABLED=false\nAPP_INITIALIZER_ENABLED=true\nAPP_INITIALIZER_ADMIN_EMAIL=admin@%s\nAPP_INITIALIZER_ADMIN_NAME=%s\nAPP_INITIALIZER_ADMIN_PASSWORD=%s\n",
		database,
		jwt,
		public,
		u.Hostname(),
		name,
		admin,
	)
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(content)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (e *execution) deploy(ctx context.Context) error {
	if e.env.Request.Operation == "deploy" {
		if e.env.Target.Ref != e.env.Request.Ref {
			return fmt.Errorf("source target ref mismatch")
		}
		if err := e.env.Target.Verify(filepath.Join(e.options.Incoming, "source.tar")); err != nil {
			return err
		}
	}
	if err := e.temp(); err != nil {
		return err
	}
	if _, err := e.output(ctx, "docker", "compose", "version"); err != nil {
		if e.options.Incoming == "" {
			return err
		}
		if err = e.run(
			ctx,
			"bash",
			[]string{filepath.Join(e.tooling, "deploy/bootstrap-host.sh")},
			nil,
			e.options.Diagnostics,
		); err != nil {
			return err
		}
	}
	if err := e.ensureEnv(); err != nil {
		return err
	}
	if e.env.Request.Operation == "reset" {
		return e.reset(ctx)
	}
	archive := filepath.Join(e.options.Incoming, "source.tar")
	if e.env.Request.Environment == "production" {
		client, err := NewJobClient(e.env.Settings)
		if err != nil {
			return err
		}
		if err = client.VerifyProduction(ctx, e.env.Target.Ref, e.env.Target.Revision); err != nil {
			return err
		}
	}
	if err := e.phase("build"); err != nil {
		return err
	}
	staged := filepath.Join(e.work, "app")
	if err := operations.ExtractSource(archive, staged, false); err != nil {
		return err
	}
	if err := writeRevision(staged, e.env.Target.Revision, e.env.Target.Ref); err != nil {
		return err
	}
	if err := e.buildApplication(ctx, staged); err != nil {
		return err
	}
	if err := e.phase("pre_deploy_backup"); err != nil {
		return err
	}
	if err := e.dumpDatabase(ctx); err != nil {
		return err
	}
	if err := e.phase("install"); err != nil {
		return err
	}
	e.data.MutationStarted = true
	if err := e.install(staged); err != nil {
		return err
	}
	e.result.Revision = e.env.Target.Revision
	e.setRef(e.env.Target.Ref)
	if err := e.compose(ctx, nil, e.options.Diagnostics, "up", "-d", "--remove-orphans"); err != nil {
		return err
	}
	e.result.Applied = true
	if err := e.phase("health"); err != nil {
		return err
	}
	if err := e.health(ctx); err != nil {
		return err
	}
	return e.waitSeed(ctx)
}

func writeRevision(app, sha, ref string) error {
	if err := os.WriteFile(filepath.Join(app, "REVISION"), []byte(sha+"\n"), 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(app, "REF"), []byte(ref+"\n"), 0600)
}

func (e *execution) buildApplication(ctx context.Context, application string) error {
	target := e.applicationTarget(application)
	for _, name := range findApplicationLayout(application).files() {
		info, err := os.Stat(name)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("revision lacks compatible Docker build")
		}
	}
	if err := e.run(ctx, "docker", target.arguments("config", "--quiet"), nil, nil); err != nil {
		return err
	}
	for _, service := range []string{"chamados-backend", "chamados-frontend"} {
		if err := e.run(
			ctx,
			"docker",
			target.arguments("build", service),
			nil,
			e.options.Diagnostics,
		); err != nil {
			return err
		}
	}
	return nil
}

func (e *execution) install(staged string) error {
	current := filepath.Join(e.options.Dir, "app")
	previous := filepath.Join(e.work, "previous-app")
	if _, err := os.Stat(current); err == nil {
		if err = os.Rename(current, previous); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(staged, current); err != nil {
		_ = os.Rename(previous, current)
		return err
	}
	e.app = current
	installed := filepath.Join(e.options.Dir, "tooling")
	if e.tooling != installed {
		if _, err := os.Stat(installed); err == nil {
			if err = os.Rename(installed, filepath.Join(e.work, "previous-tooling")); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(e.tooling, installed); err != nil {
			return err
		}
		e.tooling = installed
	}
	return nil
}

func (e *execution) dumpDatabase(ctx context.Context) error {
	if _, err := e.output(ctx, "docker", "volume", "inspect", e.project()+"_postgres_data"); err != nil {
		return nil
	}
	running, err := e.composeOutput(ctx, "ps", "-q", "--status", "running", "chamados-postgresql")
	if err != nil {
		return err
	}
	if strings.TrimSpace(running) == "" {
		if err = e.compose(
			ctx,
			nil,
			e.options.Diagnostics,
			"up",
			"-d",
			"--wait",
			"chamados-postgresql",
		); err != nil {
			return err
		}
	}
	dir := filepath.Join(e.options.Dir, "backups")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	revision := readText(filepath.Join(e.options.Dir, "app/REVISION"))
	if revision == "" {
		revision = "sem-revisao"
	}
	if strings.ContainsAny(revision, "/\\\x00\r\n") {
		return fmt.Errorf("invalid installed revision")
	}
	file := filepath.Join(dir, "pre-deploy-"+time.Now().UTC().Format("20060102150405")+"-"+revision+".sql.gz")
	f, err := os.OpenFile(file+".tmp", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(file + ".tmp")
	gz, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil {
		_ = f.Close()
		return err
	}
	err = e.compose(
		ctx,
		nil,
		gz,
		"exec",
		"-T",
		"chamados-postgresql",
		"sh",
		"-eu",
		"-c",
		`exec pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" --no-owner --no-privileges`,
	)
	for _, closer := range []io.Closer{gz, f} {
		if closeErr := closer.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		return err
	}
	if err = os.Rename(file+".tmp", file); err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(dir, "pre-deploy-*.sql.gz"))
	if err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	for i := 5; i < len(files); i++ {
		if err = os.Remove(files[i]); err != nil {
			return err
		}
	}
	return nil
}

func (e *execution) reset(ctx context.Context) error {
	if e.env.Request.Environment != "staging" {
		return fmt.Errorf("production reset prohibited")
	}
	if _, err := os.Stat(e.app); err != nil {
		return fmt.Errorf("no previous deployment")
	}
	settings, err := readEnv(filepath.Join(e.options.Dir, ".env"))
	if err != nil {
		return err
	}
	if settings["APP_INITIALIZER_ENABLED"] != "true" {
		return fmt.Errorf("reset requires APP_INITIALIZER_ENABLED=true")
	}
	if err = e.phase("reset"); err != nil {
		return err
	}
	e.data.MutationStarted = true
	if err = e.compose(ctx, nil, e.options.Diagnostics, "down", "-v", "--remove-orphans"); err != nil {
		return err
	}
	volumes, err := e.output(
		ctx,
		"docker",
		"volume",
		"ls",
		"-q",
		"--filter",
		"label=com.docker.compose.project="+e.project(),
	)
	if err != nil {
		return err
	}
	for _, volume := range strings.Fields(volumes) {
		if strings.HasPrefix(volume, "-") {
			return fmt.Errorf("invalid project volume")
		}
		if err = e.run(
			ctx,
			"docker",
			[]string{"volume", "rm", volume},
			nil,
			e.options.Diagnostics,
		); err != nil {
			return err
		}
	}
	if err = e.compose(ctx, nil, e.options.Diagnostics, "up", "-d", "--remove-orphans"); err != nil {
		return err
	}
	e.result.Applied = true
	if err = e.health(ctx); err != nil {
		return err
	}
	return e.waitSeed(ctx)
}

func (e *execution) waitSeed(ctx context.Context) error {
	settings, err := readEnv(filepath.Join(e.options.Dir, ".env"))
	if err != nil {
		return err
	}
	if settings["APP_INITIALIZER_ENABLED"] != "true" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	for {
		output, queryErr := e.composeOutput(
			ctx,
			"exec",
			"-T",
			"chamados-postgresql",
			"sh",
			"-eu",
			"-c",
			`exec psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atc 'select count(*) from users'`,
		)
		count, parseErr := strconv.Atoi(strings.TrimSpace(output))
		if queryErr == nil && parseErr == nil && count > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("seed did not complete: %w", ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
}
