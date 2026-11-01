"""Hermetic installer tests: real curl, a temporary HOME and a fake Unix API."""

import copy
from http.server import BaseHTTPRequestHandler
import json
import os
from pathlib import Path
import shutil
import socketserver
import subprocess
import tempfile
import threading
import unittest


ROOT = Path(__file__).resolve().parents[1]
WRITABLE = {
    "model", "thinking_level", "tool_overrides", "compact_nudge_tokens",
    "idle_compaction_minutes", "idle_compaction_prompt",
    "cache_keep_warm_minutes", "system_prompt", "voice_prompt",
}
LEGACY_HOOK = b"""#!/bin/sh
prompt=$(cat)
printf '%s' "$prompt"
case $prompt in
  *"You are a subagent of Shelley"*) ;;
  *) printf '\\n\\n'; cat ~/.config/roles/coordinator.md; echo; cat ~/.config/roles/coordinator-shelley.md ;;
esac
"""


def profile(name, **overrides):
    return {
        "name": name, "default": name == "Assistant",
        "builtin": name in ("Default", "Assistant"), "user_edited": False,
        "project": False, "model": "", "thinking_level": "",
        "tool_overrides": {}, "compact_nudge_tokens": 0, "system_prompt": "",
        "idle_compaction_minutes": 0, "idle_compaction_prompt": "",
        "cache_keep_warm_minutes": 0, "voice_prompt": "", **overrides,
    }


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        self.handle_api()

    def do_POST(self):
        self.handle_api()

    def do_PUT(self):
        self.handle_api()

    def handle_api(self):
        state = self.server.state
        raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        body = json.loads(raw) if raw else None
        key = (self.command, self.path)
        state["requests"].append((*key, body))
        if key in state["failures"]:
            status, result = state["failures"][key]
        elif key == ("GET", "/api/system-prompt"):
            status, result = 200, {"template": state["template"]}
        elif key == ("GET", "/api/profiles"):
            status, result = 200, state["profiles"]
        elif key == ("POST", "/api/profiles/Default/default"):
            for item in state["profiles"]:
                item["default"] = item["name"] == "Default"
            status, result = 204, None
        elif key == ("POST", "/api/profiles"):
            if set(body) - WRITABLE - {"name"} or body.get("name") != "Multi-agent":
                status, result = 400, {"error": "invalid create fields"}
            elif any(p["name"] == body["name"] for p in state["profiles"]):
                status, result = 409, {"error": "duplicate"}
            else:
                state["profiles"].append(profile(**body))
                status, result = 201, state["profiles"][-1]
        elif key == ("PUT", "/api/profiles/Multi-agent"):
            if set(body) - WRITABLE:
                status, result = 400, {"error": "invalid update fields"}
            else:
                item = next(p for p in state["profiles"] if p["name"] == "Multi-agent")
                item.update(body)
                status, result = 200, item
        else:
            status, result = 404, {"error": "unexpected endpoint"}
        if state.get("replace_hook") and self.command == "POST":
            state["replace_hook"].write_text("new user hook\n")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        if result is not None:
            try:
                self.wfile.write(result if isinstance(result, bytes) else json.dumps(result).encode())
            except BrokenPipeError:
                pass  # curl --fail may close without reading an error body.


class InstallerTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.home = Path(temporary.name)
        self.hook = self.home / ".config/shelley/hooks/system-prompt"
        self.hook.parent.mkdir(parents=True)
        self.socket = self.hook.parent.parent / "shelley.sock"
        self.state = {
            "template": "Stock {{.WorkingDir}}\n", "requests": [], "failures": {},
            "profiles": [
                profile("Assistant", system_prompt="personal", model="assistant-model"),
                profile("Default", model="custom-default", user_edited=True),
                profile("Unrelated", system_prompt="leave alone"),
            ],
        }
        server = socketserver.UnixStreamServer(str(self.socket), Handler)
        server.state = self.state
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(thread.join)
        self.addCleanup(server.shutdown)

    def run_installer(self, success=True, script=None):
        result = subprocess.run(
            [str(script or ROOT / "install-shelley-profile")],
            env={**os.environ, "HOME": str(self.home),
                 "SHELLEY_URL": "http://invalid.invalid:1",
                 "http_proxy": "http://invalid.invalid:1"},
            capture_output=True, text=True, timeout=10,
        )
        self.assertEqual(result.returncode, 0 if success else 1, result.stdout + result.stderr)
        return result

    def writes(self):
        return [r for r in self.state["requests"] if r[0] != "GET"]

    def test_create_and_rerun_preserve_settings_and_refresh_base(self):
        # Claude's installed research mode must not change Shelley's profile.
        conf = self.home / ".config/roles"
        conf.mkdir()
        (conf / "mode").write_text("research\n")
        (conf / "coordinator.md").write_text("Research coordinator, not for Shelley\n")
        before = copy.deepcopy(self.state["profiles"])
        result = self.run_installer()
        self.assertIn("Multi-agent", result.stdout)
        self.assertFalse(self.hook.exists())
        self.assertEqual(
            [(r[0], r[1]) for r in self.state["requests"]],
            [("GET", "/api/system-prompt"), ("GET", "/api/profiles"),
             ("POST", "/api/profiles"), ("POST", "/api/profiles/Default/default")],
        )
        for old, new in zip(before, self.state["profiles"]):
            self.assertEqual(
                {k: v for k, v in old.items() if k != "default"},
                {k: v for k, v in new.items() if k != "default"},
            )
        self.assertEqual([p["name"] for p in self.state["profiles"] if p["default"]], ["Default"])
        multi = self.state["profiles"][-1]
        expected = (
            "Stock {{.WorkingDir}}\n\n" + (ROOT / "modes/build.md").read_text().rstrip()
            + "\n\n" + (ROOT / "modes/shelley.md").read_text()
        )
        self.assertEqual(multi["system_prompt"], expected)
        for role in (ROOT / "roles").glob("*.md"):
            self.assertEqual((self.home / ".claude/agents" / role.name).read_bytes(), role.read_bytes())
        multi.update(
            model="chosen-model", thinking_level="high", tool_overrides={"bash": "off"},
            compact_nudge_tokens=12345, idle_compaction_minutes=17,
            idle_compaction_prompt="retain context", cache_keep_warm_minutes=20,
            voice_prompt="chosen voice",
        )
        customized = copy.deepcopy(multi)
        self.state["template"] = "Upgraded base\n"
        self.state["requests"].clear()
        self.run_installer()
        self.assertEqual(len(self.state["profiles"]), 4)
        self.assertEqual(
            {k: v for k, v in multi.items() if k != "system_prompt"},
            {k: v for k, v in customized.items() if k != "system_prompt"},
        )
        self.assertEqual(multi["system_prompt"], expected.replace("Stock {{.WorkingDir}}", "Upgraded base"))
        self.assertEqual(
            [(r[0], r[1]) for r in self.writes()],
            [("PUT", "/api/profiles/Multi-agent"), ("POST", "/api/profiles/Default/default")],
        )
        self.assertFalse(set(self.writes()[0][2]) - WRITABLE)

    def test_exact_legacy_hook_removed_after_success(self):
        self.hook.write_bytes(LEGACY_HOOK)
        self.run_installer()
        self.assertFalse(self.hook.exists())

    def test_unknown_hooks_refused_before_any_changes(self):
        for content in (b"user hook\n", LEGACY_HOOK.rstrip(b"\n"), LEGACY_HOOK + b"\n"):
            with self.subTest(content=content):
                self.hook.write_bytes(content)
                result = self.run_installer(success=False)
                self.assertIn("refusing unknown", result.stderr)
                self.assertEqual(self.hook.read_bytes(), content)
                self.assertEqual(self.state["requests"], [])
                self.assertFalse((self.home / ".claude").exists())

    def test_symlink_refused_even_with_matching_contents(self):
        target = self.home / "hook-target"
        target.write_bytes(LEGACY_HOOK)
        self.hook.symlink_to(target)
        self.run_installer(success=False)
        self.assertTrue(self.hook.is_symlink())
        self.assertEqual(self.state["requests"], [])

    def test_api_failures_keep_legacy_hook_and_do_not_continue(self):
        cases = [
            ("GET", "/api/system-prompt"), ("GET", "/api/profiles"),
            ("POST", "/api/profiles"), ("POST", "/api/profiles/Default/default"),
            ("PUT", "/api/profiles/Multi-agent"),
        ]
        original = copy.deepcopy(self.state["profiles"])
        for key in cases:
            with self.subTest(key=key):
                self.state["profiles"] = copy.deepcopy(original)
                if key[0] == "PUT":
                    self.state["profiles"].append(profile("Multi-agent"))
                self.state["requests"].clear()
                self.state["failures"] = {key: (500, {"error": "injected"})}
                self.hook.write_bytes(LEGACY_HOOK)
                result = self.run_installer(success=False)
                self.assertIn("failed via", result.stderr)
                self.assertEqual(self.hook.read_bytes(), LEGACY_HOOK)
                self.assertEqual(self.state["requests"][-1][:2], key)

    def test_invalid_api_responses_fail_without_writes(self):
        cases = [
            ("/api/system-prompt", b"not json"),
            ("/api/system-prompt", {"template": ""}),
            ("/api/system-prompt", {"template": 1}),
            ("/api/profiles", {}),
            ("/api/profiles", [profile("Assistant")]),
            ("/api/profiles", [profile("Default"), profile("Default")]),
            ("/api/profiles", [profile("Default"), {"name": "Multi-agent"}]),
        ]
        for path, response in cases:
            with self.subTest(path=path, response=response):
                self.state["requests"].clear()
                self.state["failures"] = {("GET", path): (200, response)}
                self.hook.write_bytes(LEGACY_HOOK)
                self.run_installer(success=False)
                self.assertEqual(self.writes(), [])
                self.assertEqual(self.hook.read_bytes(), LEGACY_HOOK)
                self.assertFalse((self.home / ".claude").exists())

    def test_changed_hook_is_not_deleted(self):
        self.hook.write_bytes(LEGACY_HOOK)
        self.state["replace_hook"] = self.hook
        self.run_installer(success=False)
        self.assertEqual(self.hook.read_text(), "new user hook\n")

    def test_no_socket_fails_clearly(self):
        self.socket.unlink()
        result = self.run_installer(success=False)
        self.assertIn(str(self.socket), result.stderr)
        self.assertEqual(self.writes(), [])

    def test_incomplete_repo_fails_before_requests(self):
        checkout = self.home / "checkout"
        checkout.mkdir()
        shutil.copy2(ROOT / "install-shelley-profile", checkout)
        shutil.copytree(ROOT / "modes", checkout / "modes")
        result = self.run_installer(success=False, script=checkout / "install-shelley-profile")
        self.assertIn("no roles/*.md", result.stderr)
        self.assertEqual(self.state["requests"], [])

    def test_setup_rejects_unknown_hook_before_installing_other_files(self):
        self.hook.write_bytes(b"user hook\n")
        result = subprocess.run(
            [str(ROOT / "setup"), "--mode", "research"],
            env={**os.environ, "HOME": str(self.home)},
            capture_output=True, text=True, timeout=10,
        )
        self.assertEqual(result.returncode, 1)
        self.assertIn("refusing unknown", result.stderr)
        self.assertEqual(self.state["requests"], [])
        self.assertFalse((self.home / ".config/roles").exists())
        self.assertFalse((self.home / ".claude").exists())


if __name__ == "__main__":
    unittest.main()
