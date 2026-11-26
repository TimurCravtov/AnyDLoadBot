//go:build unix

package ytdlp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// startServer starts the yt-dlp fork server (deploy/ytdlp-server/server.py)
// and keeps it running, restarting it if it exits. The returned Runner sends
// each run to the server and uses fallback, which starts yt-dlp normally,
// whenever the server can't be reached: while it is still starting, say.
func startServer(command []string, fallback Runner) Runner {
	sock := filepath.Join(os.TempDir(), "ytdlp-"+strconv.Itoa(os.Getpid())+".sock")
	go superviseServer(command, sock)

	return func(ctx context.Context, args ...string) ([]byte, error) {
		out, err, ok := runOnServer(ctx, sock, args)
		if !ok {
			return fallback(ctx, args...)
		}
		return out, err
	}
}

func superviseServer(command []string, sock string) {
	delay := time.Second
	for {
		start := time.Now()
		cmd := exec.Command(command[0], append(command[1:], sock)...)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		err := cmd.Run()
		if time.Since(start) > time.Minute {
			delay = time.Second // it ran fine for a while: restart quickly
		}
		slog.Warn("yt-dlp server stopped, restarting it; runs start yt-dlp directly meanwhile",
			"err", err, "retry_in", delay.String())
		time.Sleep(delay)
		delay = min(delay*2, time.Minute)
	}
}

// runOnServer runs yt-dlp through the fork server. ok is false if the server
// couldn't take the run, which is then safe to do another way.
func runOnServer(ctx context.Context, sock string, args []string) (out []byte, err error, ok bool) {
	conn, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: sock, Net: "unixpacket"})
	if err != nil {
		return nil, nil, false
	}
	defer conn.Close()

	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, nil, false
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, nil, false
	}
	defer outR.Close()
	defer errR.Close()

	req, _ := json.Marshal(struct {
		Args []string `json:"args"`
	}{args})
	_, _, err = conn.WriteMsgUnix(req, syscall.UnixRights(int(outW.Fd()), int(errW.Fd())), nil)
	// The server has its own copies now; ours must go, or the pipes never close.
	outW.Close()
	errW.Close()
	if err != nil {
		return nil, nil, false
	}

	var started struct {
		PID int `json:"pid"`
	}
	if err := readMsg(conn, &started); err != nil || started.PID <= 0 {
		return nil, nil, false // no child was started
	}

	// From here on the run is the server's child: kill its whole process
	// group (yt-dlp, ffmpeg, deno) if the download is cancelled.
	stop := context.AfterFunc(ctx, func() { _ = syscall.Kill(-started.PID, syscall.SIGKILL) })
	defer stop()

	stdout, stderr := drain(ctx, outR, errR)

	var finished struct {
		Exit *int `json:"exit"`
	}
	var runErr error
	switch err := readMsg(conn, &finished); {
	case err != nil || finished.Exit == nil:
		_ = syscall.Kill(-started.PID, syscall.SIGKILL)
		runErr = errors.New("lost the yt-dlp server")
	case *finished.Exit != 0:
		runErr = fmt.Errorf("exit status %d", *finished.Exit)
	}
	out, err = result(ctx, runErr, stdout, stderr)
	return out, err, true
}

func readMsg(conn *net.UnixConn, v any) error {
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return err
	}
	return json.Unmarshal(buf[:n], v)
}
