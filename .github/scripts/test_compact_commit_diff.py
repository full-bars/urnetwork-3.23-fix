import os, sys, unittest
from unittest import mock
sys.path.insert(0, os.path.dirname(__file__))
import compact_commit_diff as ccd


def _file(filename, additions=100, deletions=50, patch=None):
    return {"filename": filename, "additions": additions,
            "deletions": deletions, "patch": patch}


CFAA_BEFORE = "// gen\nconst cfaaBlockedPrefixCount = 42327\nconst cfaaBlockedPrefix6Count = 529\n"
CFAA_AFTER = "// gen\nconst cfaaBlockedPrefixCount = 42400\nconst cfaaBlockedPrefix6Count = 533\n"


class ForkAwareManifestTest(unittest.TestCase):
    def test_file_the_fork_lacks_is_marked(self):
        note = ccd.fork_aware_note("ip_blocker_block.go")
        self.assertIn("FORK LACKS", note)
        self.assertIn("ip_blocker_block.go", note)
        self.assertIn("NO ACTION", note)

    def test_mux_consumer_file_is_marked(self):
        note = ccd.fork_aware_note("ip_mux_upgrade.go")
        self.assertIn("FORK LACKS", note)
        self.assertIn("ip_mux_upgrade.go", note)
        self.assertIn("NO ACTION", note)

    def test_diverged_table_carries_counts_and_guidance(self):
        note = ccd.fork_aware_note("ip_security_cfaa_block.go")
        self.assertIn("FORK DIVERGED", note)
        self.assertIn("44225", note)
        self.assertIn("513", note)
        self.assertNotIn("not [MUST PORT].", note)
        self.assertIn("WATCH", note)
        self.assertIn("NO ACTION", note)

    def test_unknown_or_shared_code_file_gets_no_annotation(self):
        self.assertEqual(ccd.fork_aware_note("ip.go"), "")
        self.assertEqual(ccd.fork_aware_note("transfer.go"), "")

    def test_data_table_lacks_skips_fetch_and_emits_note(self):
        files = [_file("ip_blocker_block.go", additions=30142, deletions=30141)]
        with mock.patch.object(ccd, "_const_delta_str") as m:
            out = ccd._shape("urnetwork/connect", files, "aaaa", "bbbb")
            m.assert_not_called()
        self.assertIn("FORK LACKS", out)
        self.assertIn("ip_blocker_block.go", out)

    def test_no_patch_annotated_file_keeps_note(self):
        files = [_file("ip_mux_upgrade.go", additions=0, deletions=0, patch=None)]
        out = ccd._shape("urnetwork/connect", files, "aaaa", "bbbb")
        self.assertIn("<no patch in API for ip_mux_upgrade.go", out)
        self.assertIn("FORK LACKS", out)

    def test_regular_code_file_with_annotation_prepends_note(self):
        orig = ccd.FORK_AWARE
        try:
            ccd.FORK_AWARE = {**orig, "ip_mux_upgrade.go": "FORK DIVERGED placeholder"}
            p = "diff --git a/ip_mux_upgrade.go b/ip_mux_upgrade.go\n@@ -1 +1 @@\n-//a\n+//b\n"
            files = [_file("ip_mux_upgrade.go", patch=p)]
            out = ccd._shape("urnetwork/connect", files, "aaaa", "bbbb")
            self.assertIn("# ip_mux_upgrade.go", out)
            self.assertIn("FORK DIVERGED placeholder", out)
            self.assertLess(out.find("FORK DIVERGED placeholder"), out.find("diff --git"))
        finally:
            ccd.FORK_AWARE = orig

    def test_diverged_cfaa_commit_mode_emits_note_and_const_delta(self):
        files = [_file("ip_security_cfaa_block.go", additions=10235, deletions=10199)]
        with mock.patch.object(ccd, "fetch_file",
                               side_effect=lambda r, sha, p:
                               CFAA_BEFORE if sha == "aaaa" else CFAA_AFTER):
            out = ccd._shape("urnetwork/connect", files, "aaaa", "bbbb")
        self.assertIn("FORK DIVERGED", out)
        self.assertIn("data-table ip_security_cfaa_block.go", out)
        self.assertIn("42327 -> 42400", out)

    def test_diverged_cfaa_pr_mode_no_const_delta_no_fetch(self):
        files = [_file("ip_security_cfaa_block.go", additions=10, deletions=5)]
        with mock.patch.object(ccd, "fetch_file") as m:
            out = ccd._shape("urnetwork/connect", files, None, None)
            m.assert_not_called()
        self.assertIn("FORK DIVERGED", out)
        self.assertIn("<const diff unavailable", out)
        self.assertNotIn("FORK LACKS", out)


if __name__ == "__main__":
    unittest.main(verbosity=2)
