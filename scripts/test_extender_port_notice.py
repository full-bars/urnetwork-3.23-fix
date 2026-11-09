#!/usr/bin/env python3
"""Exercise the extracted install-time extender notice and offer, never an installer or entrypoint.

report_extender_port only prints. It never asks a question and never fails the
install: the extender is on by default, so its job is to say what will happen.
offer_extender_capability is the one place that may ask: it offers a single
`sudo setcap` when this user cannot bind TCP 443, and only with a terminal.
"""
import os
import pty
import select
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
INSTALLER = "scripts/Provider_Install_Linux.sh"


def subshell_fn(name):
    text = (ROOT / INSTALLER).read_text()
    start = text.index(name + " ()\n(\n")
    end = text.index("\n)\n", start) + 3
    return text[start:end]


def what_note():
    text = (ROOT / INSTALLER).read_text()
    start = text.index("extender_what_note ()\n{\n")
    end = text.index("\n}\n", start) + 3
    return text[start:end]


def ports_note():
    text = (ROOT / INSTALLER).read_text()
    start = text.index("extender_ports_note ()\n{\n")
    end = text.index("\n}\n", start) + 3
    return text[start:end]


def helper():
    return subshell_fn("report_extender_port")


def base_env(mock):
    for tool in ("sh", "sed", "head", "awk", "cat", "grep"):
        executable = shutil.which(tool)
        if executable is None:
            raise RuntimeError(f"missing test dependency: {tool}")
        (mock / tool).symlink_to(executable)


class ExtenderPortNoticeTest(unittest.TestCase):
    def run_notice(self, ss_output="", ss_present=True, uid="1000", cap_state="cap_net_bind_service=ep", platform="Linux", ss_exit="0", exe_of_pid=None):
        with tempfile.TemporaryDirectory() as tmp:
            mock = Path(tmp) / "bin"
            mock.mkdir()
            base_env(mock)
            if ss_present:
                ss = mock / "ss"
                ss.write_text("#!/bin/sh\nprintf '%s' \"$SS_OUTPUT\"\nexit \"$SS_EXIT\"\n")
                ss.chmod(0o755)
            binary = Path(tmp) / "urnetwork"
            binary.touch()
            env = dict(os.environ, PATH=str(mock), SS_OUTPUT=ss_output, UID_VALUE=uid,
                       CAP_STATE=cap_state, PLATFORM=platform, BINARY=str(binary), SS_EXIT=ss_exit,
                       EXE_OF_PID=(str(binary) + " (deleted)" if exe_of_pid == "DELETED" else exe_of_pid if exe_of_pid is not None else str(binary)), EXE_UNREADABLE="1" if exe_of_pid == "" else "0")
            harness = r'''
set -eu
uname() { printf '%s\n' "$PLATFORM"; }
id() { printf '%s\n' "$UID_VALUE"; }
getcap() { printf '%s %s\n' "$1" "$CAP_STATE"; }
pr_info() { printf 'INFO: %s\n' "$*"; }
pr_warn() { printf 'WARN: %s\n' "$*"; }
readlink() {
    case "$2" in
        /proc/*/exe) [ "$EXE_UNREADABLE" = 1 ] && return 1; printf '%s\n' "$EXE_OF_PID" ;;
        *) printf '%s\n' "$2" ;;
    esac
}
''' + ports_note() + what_note() + helper() + '\nreport_extender_port "$BINARY"\n'
            result = subprocess.run(["sh", "-c", harness], env=env, text=True, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            return result.stdout

    def test_free_port_says_the_extender_will_be_on_and_how_to_disable_it(self):
        out = self.run_notice(ss_output="")
        self.assertIn("INFO:", out)
        self.assertIn("443 is free", out)
        self.assertIn("on by default", out)
        self.assertIn("urnet-tools extender disable", out)
        self.assertNotIn("WARN:", out)

    def test_the_notice_says_the_extender_does_not_change_earnings(self):
        out = self.run_notice(ss_output="")
        self.assertIn("does not change your earnings", out)
        self.assertIn("censored countries", out)

    def test_ports_note_separates_required_from_optional(self):
        out = self.run_notice(ss_output="")
        self.assertIn("TCP 443   required", out)
        self.assertIn("UDP 443   optional", out)
        self.assertIn("UDP 4053  optional", out)
        self.assertIn("UDP 53    optional", out)
        self.assertNotIn("DNS53", out)  # beginners cannot act on an env var
        self.assertIn("urnet-tools extender setup", out)
        self.assertIn("answers DNS", out)
        self.assertIn("provider itself needs none", out)

    def test_taken_port_names_the_holder_and_says_it_will_keep_retrying(self):
        out = self.run_notice(ss_output='LISTEN 0 4096 *:443 *:* users:(("caddy",pid=894,fd=7))\n')
        self.assertIn("WARN:", out)
        self.assertIn("caddy (pid 894)", out)
        self.assertIn("keeps retrying", out)
        self.assertIn("urnet-tools extender disable", out)

    def test_taken_by_a_process_whose_name_is_hidden_says_how_to_find_it(self):
        out = self.run_notice(ss_output="LISTEN 0 4096 *:443 *:*\n")
        self.assertIn("WARN:", out)
        self.assertIn("sudo ss -ltnp", out)

    def test_an_update_does_not_blame_the_providers_own_listener(self):
        # the listener's executable is this provider's binary
        out = self.run_notice(ss_output='LISTEN 0 4096 *:443 *:* users:(("urnetwork",pid=10,fd=9))\n')
        self.assertNotIn("WARN:", out)
        self.assertIn("already", out)

    def test_an_update_replaced_binary_is_still_the_providers_own_listener(self):
        # an update replaces the file, so the running process's exe reads "(deleted)"
        out = self.run_notice(ss_output='LISTEN 0 4096 *:443 *:* users:(("urnetwork",pid=10,fd=9))\n',
                              exe_of_pid="DELETED")
        self.assertIn("already", out)

    def test_a_different_program_named_urnetwork_is_not_blamed_on_this_provider(self):
        out = self.run_notice(ss_output='LISTEN 0 4096 *:443 *:* users:(("urnetwork",pid=10,fd=9))\n',
                              exe_of_pid="/opt/other/urnetwork")
        self.assertIn("WARN:", out)
        self.assertIn("urnetwork (pid 10)", out)
        self.assertNotIn("already listening", out)

    def test_an_unreadable_executable_is_said_not_assumed(self):
        out = self.run_notice(ss_output='LISTEN 0 4096 *:443 *:* users:(("urnetwork",pid=10,fd=9))\n', exe_of_pid="")
        self.assertIn("WARN:", out)
        self.assertIn("could not confirm", out)

    def test_a_failed_ss_is_not_reported_as_a_free_port(self):
        out = self.run_notice(ss_output="", ss_exit="1")
        self.assertNotIn("TCP 443 is free, so", out)
        self.assertIn("Could not check", out)
        self.assertIn("urnet-tools extender disable", out)

    def test_missing_capability_is_left_to_the_offer_not_repeated_here(self):
        # report only states the port; offer_extender_capability explains the capability once
        out = self.run_notice(ss_output="", uid="1000", cap_state="")
        self.assertNotIn("setcap", out)
        self.assertNotIn("TCP 443   required", out)

    def test_root_needs_no_capability_and_gets_the_ports_note(self):
        out = self.run_notice(ss_output="", uid="0", cap_state="")
        self.assertNotIn("WARN:", out)
        self.assertIn("TCP 443   required", out)

    def test_without_ss_it_still_explains_and_does_not_fail(self):
        out = self.run_notice(ss_present=False)
        self.assertIn("on by default", out)
        self.assertIn("Could not check", out)

    def test_other_platforms_print_nothing(self):
        self.assertEqual(self.run_notice(platform="Darwin"), "")


class ExtenderCapabilityOfferTest(unittest.TestCase):
    def run_offer(self, answer="y", uid="1000", has_cap=False, tty="ok", force="0",
                  sudo="ok", platform="Linux", piped=False):
        """tty: "ok" (a file holding the answer), "none" (nothing to open).
        sudo: "ok" (setcap succeeds and sets the capability), "fail", "absent"."""
        with tempfile.TemporaryDirectory() as tmp:
            mock = Path(tmp) / "bin"
            mock.mkdir()
            base_env(mock)
            binary = Path(tmp) / "urnetwork"
            binary.touch()
            tty_file = Path(tmp) / "tty"
            tty_file.write_text(answer + "\n")
            capfile = Path(tmp) / "cap"
            if has_cap:
                capfile.write_text("set")
            mark = Path(tmp) / "sudo-called"
            env = dict(os.environ, PATH=str(mock), UID_VALUE=uid, PLATFORM=platform,
                       BINARY=str(binary), CAPFILE=str(capfile), MARK=str(mark),
                       SUDO_MODE=sudo, FORCE=force,
                       extender_tty=str(tty_file) if tty == "ok" else str(Path(tmp) / "no-such-tty"))
            sudo_fn = "" if sudo == "absent" else r'''
sudo() {
    printf called > "$MARK"
    [ "$SUDO_MODE" = ok ] || return 1
    printf set > "$CAPFILE"
}
'''
            harness = r'''
set -eu
uname() { printf '%s\n' "$PLATFORM"; }
id() { printf '%s\n' "$UID_VALUE"; }
getcap() { [ -f "$CAPFILE" ] && printf '%s cap_net_bind_service=ep\n' "$1"; return 0; }
pr_info() { printf 'INFO: %s\n' "$*"; }
pr_warn() { printf 'WARN: %s\n' "$*"; }
''' + sudo_fn + ports_note() + subshell_fn("offer_extender_capability") + '\noffer_extender_capability "$BINARY"\n'
            if piped:
                result = subprocess.run(["sh", "-c", harness], env=env, text=True, capture_output=True)
                output = result.stdout
            else:
                master, slave = pty.openpty()
                try:
                    result = subprocess.run(["sh", "-c", harness], env=env, stdin=slave, stdout=slave, stderr=subprocess.PIPE, text=True)
                    os.close(slave)
                    slave = None
                    chunks = []
                    while True:
                        try:
                            data = os.read(master, 16384)
                            if not data:
                                break
                            chunks.append(data)
                        except OSError:
                            break
                    output = b"".join(chunks).decode(errors="replace").replace("\r\n", "\n")
                finally:
                    os.close(master)
                    if slave is not None:
                        os.close(slave)
            self.assertEqual(result.returncode, 0, result.stderr)
            return output, mark.exists()

    def test_pipe_does_not_prompt_even_if_controlling_terminal_exists(self):
        out, called = self.run_offer(piped=True)
        self.assertFalse(called)
        self.assertNotIn("[y/N]", out)
        self.assertIn("To allow it, run once:", out)

    def test_it_says_only_the_extender_needs_this_and_asks_before_using_sudo(self):
        out, sudo_called = self.run_offer(answer="n")
        self.assertIn("WARN:", out)
        self.assertIn("not running yet", out)
        self.assertIn("provider itself is not affected", out)
        self.assertIn("Run it now? [y/N]", out)
        self.assertIn("sudo setcap cap_net_bind_service=+ep", out)
        self.assertFalse(sudo_called)

    def test_declining_skips_without_calling_sudo_and_names_the_later_command(self):
        for answer in ("n", "", "no", "maybe"):
            out, sudo_called = self.run_offer(answer=answer)
            self.assertFalse(sudo_called, answer)
            self.assertIn("Skipped", out, answer)
            self.assertIn("The provider runs normally", out, answer)
            self.assertIn("allow the extender later", out, answer)
            self.assertNotIn("TCP 443   required", out, answer)

    def test_accepting_runs_sudo_once_and_then_explains_the_ports(self):
        out, sudo_called = self.run_offer(answer="y")
        self.assertTrue(sudo_called)
        self.assertIn("port 443 is allowed", out)
        self.assertIn("TCP 443   required", out)
        self.assertIn("UDP 4053  optional", out)
        self.assertIn("removes this permission", out)
        self.assertNotIn("did not succeed", out)

    def test_a_failed_sudo_is_said_and_never_fails_the_install(self):
        out, sudo_called = self.run_offer(answer="y", sudo="fail")
        self.assertTrue(sudo_called)
        self.assertIn("did not succeed", out)
        self.assertIn("provider is unaffected", out)
        self.assertNotIn("TCP 443   required", out)

    def test_no_terminal_prints_the_command_and_never_prompts(self):
        out, sudo_called = self.run_offer(tty="none")
        self.assertFalse(sudo_called)
        self.assertNotIn("[y/N]", out)
        self.assertIn("To allow it, run once: sudo setcap cap_net_bind_service=+ep", out)

    def test_force_never_prompts_even_with_a_terminal(self):
        out, sudo_called = self.run_offer(force="1")
        self.assertFalse(sudo_called)
        self.assertNotIn("[y/N]", out)
        self.assertIn("To allow it, run once:", out)

    def test_without_sudo_it_prints_the_command(self):
        out, _ = self.run_offer(sudo="absent")
        self.assertNotIn("[y/N]", out)
        self.assertIn("To allow it, run once:", out)

    def test_nothing_is_printed_when_the_capability_is_already_set(self):
        out, sudo_called = self.run_offer(has_cap=True)
        self.assertEqual(out, "")
        self.assertFalse(sudo_called)

    def test_nothing_is_printed_for_root(self):
        out, sudo_called = self.run_offer(uid="0")
        self.assertEqual(out, "")
        self.assertFalse(sudo_called)

    def test_other_platforms_print_nothing(self):
        out, sudo_called = self.run_offer(platform="Darwin")
        self.assertEqual(out, "")
        self.assertFalse(sudo_called)


if __name__ == "__main__":
    unittest.main()
