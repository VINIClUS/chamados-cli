"""Loopback-only SSH host with an ephemeral verified key and isolated /opt namespace."""

import json
import shlex
import socket
import subprocess
import threading
import time

import paramiko


class LocalSSH:
    def __init__(self, fixture):
        self.fixture = fixture
        self.key = paramiko.RSAKey.generate(2048)
        self.client_key = paramiko.RSAKey.generate(2048)
        self.drop = ""
        self.inspect_failure = False
        self.calls = []
        self.processes = []
        self.errors = []
        self.transports = []
        self.workers = []
        self.closed = threading.Event()
        self.socket = socket.socket()
        self.socket.bind(("127.0.0.1", 0))
        self.socket.listen()
        self.socket.settimeout(0.1)
        port = self.socket.getsockname()[1]
        self.client_key.write_private_key_file(str(fixture.root / "client-key"))
        (fixture.root / "client-key").chmod(0o600)
        known = fixture.root / "known_hosts"
        known.write_text(f"[127.0.0.1]:{port} {self.key.get_name()} {self.key.get_base64()}\n")
        home = fixture.root / "home"
        (home / ".ssh").mkdir(parents=True)
        config = home / ".ssh/config"
        config.write_text(
            f"Host 127.0.0.1\n Port {port}\n User fixture\n UserKnownHostsFile {known}\n LogLevel ERROR\n"
        )
        # OpenSSH reads config through the OS account's home, not HOME. A wrapper supplies -F explicitly.
        ssh = fixture.bin / "ssh"
        ssh.write_text(f'#!/bin/sh\nexec /usr/bin/ssh -F {shlex.quote(str(config))} "$@"\n')
        ssh.chmod(0o700)
        self.thread = threading.Thread(target=self.serve, daemon=True)
        self.thread.start()

    def serve(self):
        owner = self

        class Server(paramiko.ServerInterface):
            def check_auth_publickey(self, username, key):
                return (
                    paramiko.AUTH_SUCCESSFUL
                    if username == "fixture" and key == owner.client_key
                    else paramiko.AUTH_FAILED
                )

            def get_allowed_auths(self, username):
                return "publickey"

            def check_channel_request(self, kind, chanid):
                return (
                    paramiko.OPEN_SUCCEEDED if kind == "session" else paramiko.OPEN_FAILED_ADMINISTRATIVELY_PROHIBITED
                )

            def check_channel_exec_request(self, channel, command):
                worker = threading.Thread(target=owner.guarded_execute, args=(channel, command.decode()), daemon=True)
                owner.workers.append(worker)
                worker.start()
                return True

        while not self.closed.is_set():
            try:
                connection, _ = self.socket.accept()
            except TimeoutError:
                continue
            except OSError:
                return
            transport = paramiko.Transport(connection)
            self.transports.append(transport)
            transport.add_server_key(self.key)
            try:
                transport.start_server(server=Server())
            except EOFError, ConnectionResetError:
                # OpenSSH aborts the handshake when the known host key differs.
                # Keep accepting the next independent connection.
                transport.close()
            except paramiko.SSHException as error:
                self.errors.append(str(error))

    def guarded_execute(self, channel, remote):
        try:
            self.execute(channel, remote)
        except Exception as error:
            self.errors.append(str(error))
            try:
                channel.send_exit_status(1)
            finally:
                channel.close()

    def execute(self, channel, remote):
        self.calls.append(remote)
        if "internal inspect '" in remote and self.inspect_failure:
            channel.send_exit_status(255)
            channel.close()
            return
        translated = remote.replace("/opt/chamados", str(self.fixture.host))
        if "internal inspect '" in remote:
            translated += " --deploy-dir " + shlex.quote(str(self.fixture.host))
        env = dict(
            self.fixture.env,
            CHAMADOS_DEPLOY_DIR=str(self.fixture.host),
            BACKUP_COMPOSE_PROJECT=self.fixture.project,
            BACKUP_BACKEND_HEALTH_URL=self.fixture.api.url,
            BACKUP_FRONTEND_HEALTH_URL=self.fixture.api.url,
            BACKUP_HEALTH_TIMEOUT="1",
        )
        payload = bytearray()
        while True:
            part = channel.recv(65536)
            if not part:
                break
            payload.extend(part)
        if "execute --remote" in remote:
            operation_id = json.loads(payload)["operation_id"]
            journal = self.fixture.host / "operations" / (operation_id + ".json")
            self.fixture.control["journal"] = str(journal)
            self.fixture.api.journal_path = journal
            self.fixture.save_control()
        process = subprocess.Popen(
            ["sh", "-c", translated],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
            start_new_session=True,
        )
        self.processes.append(process)
        if "execute --remote" in remote and self.drop == "running":
            # Delay at the daemon boundary. Observe the durable safety barrier before losing the channel.
            assert process.stdin is not None
            process.stdin.write(payload)
            process.stdin.close()
            process.stdin = None
            operation_id = json.loads(payload)["operation_id"]
            journal = self.fixture.host / "operations" / (operation_id + ".json")
            deadline = time.monotonic() + 20
            while time.monotonic() < deadline:
                record = json.loads(journal.read_text()) if journal.exists() else {}
                result = record.get("result", {})
                if result.get("phase") == "replace" and result.get("data", {}).get("safety_backup_id"):
                    break
                if process.poll() is not None:
                    raise AssertionError("operation completed before loss injection at safety barrier")
                time.sleep(0.02)
            else:
                raise AssertionError("current operation never reached its safety barrier")
            channel.close()
            process.communicate(timeout=40)
            return
        output, errors = process.communicate(bytes(payload), timeout=40)
        try:
            channel.sendall(output)
            channel.sendall_stderr(errors)
            channel.send_exit_status(
                255 if "execute --remote" in remote and self.drop == "terminal" else process.returncode
            )
        except OSError:
            pass
        finally:
            channel.close()

    def close(self):
        self.closed.set()
        self.socket.close()
        for transport in self.transports:
            transport.close()
        self.thread.join(timeout=2)
        for worker in self.workers:
            worker.join(timeout=45)
        assert all(p.poll() is not None for p in self.processes), "SSH fixture leaked a child process"
        assert not self.errors, self.errors
