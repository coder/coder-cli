package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cdr.dev/coder-cli/ci/tcli"
	"cdr.dev/slog/sloggers/slogtest/assert"
)

func build(path string) error {
	cmd := exec.Command(
		"sh", "-c",
		fmt.Sprintf("cd ../../ && go build -o %s ./cmd/coder", path),
	)
	cmd.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")

	_, err := cmd.CombinedOutput()
	if err != nil {
		return err
	}
	return nil
}

var binpath string

func init() {
	cwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	binpath = filepath.Join(cwd, "bin", "coder")
	err = build(binpath)
	if err != nil {
		panic(err)
	}
}

func TestTCli(t *testing.T) {
	ctx := context.Background()

	container, err := tcli.NewContainerRunner(ctx, &tcli.ContainerConfig{
		Image: "ubuntu:latest",
		Name:  "test-container",
		BindMounts: map[string]string{
			binpath: "/bin/coder",
		},
	})
	assert.Success(t, "new run container", err)
	defer container.Close()

	container.Run(ctx, "echo testing").Assert(t,
		tcli.Success(),
		tcli.StderrEmpty(),
		tcli.StdoutMatches("esting"),
	)

	container.Run(ctx, "sleep 1.5 && echo 1>&2 stderr-message").Assert(t,
		tcli.Success(),
		tcli.StdoutEmpty(),
		tcli.StderrMatches("message"),
		tcli.DurationGreaterThan(time.Second),
	)

	cmd := exec.CommandContext(ctx, "cat")
	cmd.Stdin = strings.NewReader("testing")

	container.RunCmd(cmd).Assert(t,
		tcli.Success(),
		tcli.StderrEmpty(),
		tcli.StdoutMatches("testing"),
	)

	container.Run(ctx, "which coder").Assert(t,
		tcli.Success(),
		tcli.StdoutMatches("/bin/coder"),
		tcli.StderrEmpty(),
	)

	container.Run(ctx, "coder version").Assert(t,
		tcli.StderrEmpty(),
		tcli.Success(),
		tcli.StdoutMatches("linux"),
	)
}

func TestCoderCLI(t *testing.T) {
	ctx := context.Background()

	c, err := tcli.NewContainerRunner(ctx, &tcli.ContainerConfig{
		Image: "ubuntu:latest",
		Name:  "test-container",
		BindMounts: map[string]string{
			binpath: "/bin/coder",
		},
	})
	assert.Success(t, "new run container", err)
	defer c.Close()

	c.Run(ctx, "coder version").Assert(t,
		tcli.StderrEmpty(),
		tcli.Success(),
		tcli.StdoutMatches("linux"),
	)

	c.Run(ctx, "coder help").Assert(t,
		tcli.Success(),
		tcli.StderrMatches("Commands:"),
		tcli.StderrMatches("Usage: coder"),
		tcli.StdoutEmpty(),
	)
}
