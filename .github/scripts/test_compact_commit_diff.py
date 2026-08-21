#!/usr/bin/env python3
"""Tests for compact_commit_diff.py's fork-aware manifest behavior."""
import os
import sys
import unittest
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

    def test_diverged_table_carries_fork_counts_and_per_constant_guidance(self):
        note = ccd.fork_aware_note("ip_security_cfaa_block.go")
        self.assertIn("FORK DIVERGED", note)
        self.assertIn("44225", note)   # fork IPv4 count
        self.assertIn("513", note)     # fork IPv6 count
        # must NOT blanket-assert the fork leads with a never-port rule
        self.assertNotIn("not [MUST PORT].", note)
        # must direct per-constant judgement between WATCH and NO ACTION
        self.assertIn("WATCH", note)
        self.assertIn("NO ACTION", note)

    def test_unknown_or_shared_code_file_gets_no_annotation(self):
        self.assertEqual(ccd.fork_aware_note("ip.go"), "")
        self.assertEqual(ccd.fork_aware_note("transfer.go"), "")

    def test_data_table_lacks_shape_emits_note_and_skips_fetch(self):
        files = [_file("ip_blocker_block.go", additions=30142, deletions=30141)]
        with mock.patch.object(ccd, "_const_delta_str") as m:
            out = ccd._shape("urnetwork/connect", files, "aaaa", "bbbb")
            m.assert_not_called()  # fork-LACKS table never needs the fetch
        self.assertIn("FORK LACKS", out)
        self.assertIn("ip_blocker_block.go", out)

    def test_diverged_cfaa_commit_mode_emits_note_and_const_delta(self):
        files = [_file("ip_security_cfaa_block.go", additions=10235, deletions=10199)]
        with mock.patch.object(ccd, "fetch_file",
                               side_effect=lambda repo, sha, path:
                               CFAA_BEFORE if sha == "aaaa" else CFAA_AFTER):
            out = ccd._shape("urnetwork/connect", files, "aaaa", "bbbb")
        self.assertIn("FORK DIVERGED", out)
        self.assertIn("data-table ip_security_cfaa_block.go", out)
        self.assertIn("42327 -> 42400", out)

    def test_diverged_cfaa_pr_mode_no_const_delta_no_fetch(self):
        # PR mode passes sha_a=sha_b=None: the DIVERGED table must NOT attempt
        # a fetch, and the fallback wording must NOT call it fork-LACKS.
        files = [_file("ip_security_cfaa_block.go", additions=10, deletions=5)]
        with mock.patch.object(ccd, "fetch_file") as m:
            out = ccd._shape("urnetwork/connect", files, None, None)
            m.assert_not_called()
        self.assertIn("FORK DIVERGED", out)
        self.assertIn("<const diff unavailable", out)
        self.assertNotIn("fork-LACKS data table", out)  # regression guard for F2
        self.assertIn("data-table ip_security_cfaa_block.go", out)


if __name__ == "__main__":
    unittest.main(verbosity=2)
