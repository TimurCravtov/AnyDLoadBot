"""Fork server for yt-dlp.

Starting yt-dlp takes about two seconds (Python, its extractors, curl_cffi,
plugins). This server pays that once: it imports yt-dlp, then for every run
forks a child that already has everything loaded and runs yt-dlp's command
line in it, exactly like the yt-dlp program would.

Protocol, over a SOCK_SEQPACKET Unix socket (one message per send):
  client -> server  {"args": [...]} with two file descriptors attached
                    (SCM_RIGHTS): the run's stdout and stderr
  server -> client  {"pid": N}     the child, which leads its own process
                                   group, so the client can kill the run
  server -> client  {"exit": N}    yt-dlp's exit code, when the child ends

Usage: python server.py SOCKET_PATH
"""

import json
import os
import selectors
import signal
import socket
import sys
import traceback


def preload():
    """Import yt-dlp and what its runs need, so children start instantly."""
    import yt_dlp

    try:
        from yt_dlp.plugins import load_all_plugins

        load_all_plugins()
    except Exception:  # older or newer plugin API: the child loads them itself
        pass
    for name in (
        "curl_cffi",
        "yt_dlp.networking._curlcffi",
        "yt_dlp_ejs",
        "yt_dlp.extractor.youtube",
        "yt_dlp.extractor.tiktok",
        "yt_dlp.extractor.instagram",
        "yt_dlp.extractor.reddit",
        "yt_dlp.extractor.twitter",
        "yt_dlp.extractor.pinterest",
        "yt_dlp.postprocessor",
    ):
        try:
            __import__(name)
        except Exception:
            pass
    return yt_dlp


def run_child(yt_dlp, args, out_fd, err_fd):
    """In the forked child: become yt-dlp with the given stdout/stderr."""
    os.setsid()  # its own process group: the client kills ffmpeg/deno with it
    for sig in (signal.SIGCHLD, signal.SIGTERM, signal.SIGINT, signal.SIGPIPE):
        signal.signal(sig, signal.SIG_DFL)
    signal.set_wakeup_fd(-1)

    devnull = os.open(os.devnull, os.O_RDONLY)
    os.dup2(devnull, 0)
    os.dup2(out_fd, 1)
    os.dup2(err_fd, 2)
    for fd in (devnull, out_fd, err_fd):
        if fd > 2:
            os.close(fd)

    code = 0
    try:
        sys.argv = ["yt-dlp", *args]
        yt_dlp.main(args)
    except SystemExit as e:
        if e.code is None:
            code = 0
        elif isinstance(e.code, int):
            code = e.code
        else:
            print(e.code, file=sys.stderr)
            code = 1
    except BaseException:
        traceback.print_exc()
        code = 1
    finally:
        for stream in (sys.stdout, sys.stderr):
            try:
                stream.flush()
            except Exception:
                pass
    os._exit(code)


def send(conn, msg):
    try:
        conn.send(json.dumps(msg).encode())
    except OSError:
        pass  # the client went away; it kills the run itself


def main():
    if len(sys.argv) != 2:
        sys.exit("usage: server.py SOCKET_PATH")
    path = sys.argv[1]

    yt_dlp = preload()

    try:
        os.unlink(path)
    except FileNotFoundError:
        pass
    listener = socket.socket(socket.AF_UNIX, socket.SOCK_SEQPACKET)
    old_umask = os.umask(0o077)  # only this user may connect
    listener.bind(path)
    os.umask(old_umask)
    listener.listen(64)
    listener.setblocking(False)

    # SIGCHLD wakes the loop through this pipe; children are reaped there.
    wake_r, wake_w = os.pipe()
    os.set_blocking(wake_r, False)
    os.set_blocking(wake_w, False)
    signal.set_wakeup_fd(wake_w)
    signal.signal(signal.SIGCHLD, lambda *_: None)
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))

    sel = selectors.DefaultSelector()
    sel.register(listener, selectors.EVENT_READ, "accept")
    sel.register(wake_r, selectors.EVENT_READ, "wake")
    running = {}  # pid -> client connection

    print(f"yt-dlp server ready on {path}", file=sys.stderr, flush=True)
    while True:
        for key, _ in sel.select():
            if key.data == "accept":
                try:
                    conn, _ = listener.accept()
                except BlockingIOError:
                    continue
                sel.register(conn, selectors.EVENT_READ, "request")

            elif key.data == "request":
                conn = key.fileobj
                sel.unregister(conn)
                try:
                    conn.setblocking(True)
                    data, fds, _, _ = socket.recv_fds(conn, 1 << 20, 2)
                except OSError:
                    conn.close()
                    continue
                if not data or len(fds) != 2:
                    for fd in fds:
                        os.close(fd)
                    conn.close()
                    continue
                try:
                    args = json.loads(data)["args"]
                except Exception:
                    for fd in fds:
                        os.close(fd)
                    conn.close()
                    continue

                pid = os.fork()
                if pid == 0:
                    listener.close()
                    conn.close()
                    for other in running.values():
                        other.close()
                    run_child(yt_dlp, [str(a) for a in args], fds[0], fds[1])
                for fd in fds:
                    os.close(fd)
                running[pid] = conn
                send(conn, {"pid": pid})

            elif key.data == "wake":
                try:
                    while os.read(wake_r, 4096):
                        pass
                except BlockingIOError:
                    pass
                while True:
                    try:
                        pid, status = os.waitpid(-1, os.WNOHANG)
                    except ChildProcessError:
                        break
                    if pid == 0:
                        break
                    conn = running.pop(pid, None)
                    if conn is not None:
                        send(conn, {"exit": os.waitstatus_to_exitcode(status)})
                        conn.close()


if __name__ == "__main__":
    main()
