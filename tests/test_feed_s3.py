"""Tests for bench/feed_s3.py message-construction core (S3->S3 feeder).

Only the pure functions are tested (no boto3, no network):
- list_source_keys parsing is exercised indirectly via build_entry mapping;
- build_entry: s3: source mapping + unique-key contract (run/worker/seq) that
  prevents rclone from skipping an already-existing destination.
"""
import json

import pytest

from bench.feed_s3 import build_entry

pytestmark = pytest.mark.unit


class TestBuildEntry:
    def _entry(self, **over):
        kw = dict(src_bucket="srcbkt", key="stress-large/r0/a/b/file.bin",
                  dest_bucket="dstbkt", dest_prefix="s3test",
                  run_id="1700000000", worker_id=0, seq=0)
        kw.update(over)
        return build_entry(**kw)

    def test_source_maps_to_s3_remote(self):
        body = json.loads(self._entry()["MessageBody"])
        assert body["source"] == "s3:srcbkt/stress-large/r0/a/b/file.bin"

    def test_destination_uses_basename_not_full_key(self):
        body = json.loads(self._entry(key="a/b/c/file.bin")["MessageBody"])
        assert body["destination"].endswith("/file.bin")
        assert "a/b/c" not in body["destination"]

    def test_destination_embeds_run_worker_seq(self):
        body = json.loads(
            self._entry(key="x/f.bin", run_id="RID", worker_id=7, seq=42)["MessageBody"]
        )
        assert body["destination"] == "s3:dstbkt/s3test/run_RID/w7_42/f.bin"

    def test_key_without_slash_uses_whole_key_as_name(self):
        body = json.loads(self._entry(key="flatname")["MessageBody"])
        assert body["destination"].endswith("/flatname")

    def test_entry_id_encodes_worker_and_seq(self):
        assert self._entry(worker_id=3, seq=9)["Id"] == "w3n9"

    def test_body_is_valid_json_only_src_dst(self):
        body = json.loads(self._entry()["MessageBody"])
        assert set(body) == {"source", "destination"}

    def test_same_bucket_src_and_dest_ok(self):
        # 源和目标同桶(不同前缀)是预期用法
        body = json.loads(self._entry(src_bucket="b", dest_bucket="b")["MessageBody"])
        assert body["source"].startswith("s3:b/")
        assert body["destination"].startswith("s3:b/")
        assert body["source"] != body["destination"]


class TestUniqueKeyContract:
    def _dest(self, **over):
        kw = dict(src_bucket="b", key="src/f.bin", dest_bucket="b",
                  dest_prefix="s3test", run_id="R", worker_id=0, seq=0)
        kw.update(over)
        return json.loads(build_entry(**kw)["MessageBody"])["destination"]

    def test_distinct_seq_yields_distinct_destination(self):
        assert self._dest(seq=0) != self._dest(seq=1)

    def test_distinct_worker_yields_distinct_destination(self):
        assert self._dest(worker_id=0) != self._dest(worker_id=1)

    def test_distinct_run_id_yields_distinct_destination(self):
        assert self._dest(run_id="A") != self._dest(run_id="B")

    def test_full_grid_all_destinations_unique(self):
        dests = {
            self._dest(run_id=r, worker_id=w, seq=s)
            for r in ("R1", "R2") for w in range(4) for s in range(25)
        }
        assert len(dests) == 2 * 4 * 25

    def test_deterministic(self):
        assert self._dest(worker_id=2, seq=5) == self._dest(worker_id=2, seq=5)
