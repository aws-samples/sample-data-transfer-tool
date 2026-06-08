"""Tests for bench/feed_local.py message-construction core (task #3).

The feeder loop is I/O-bound (SQS batch send) but its *message construction*
is pure: source/dest key mapping, size routing via MessageAttributes, and the
unique-key contract that prevents rclone from skipping an already-existing
destination (which would zero out real transfer throughput → bogus bench numbers).

We test only the extracted pure functions; no boto3, no network.
"""
import json

import pytest

from bench.feed_local import build_copy_entry, parse_large_files

pytestmark = pytest.mark.unit


# ───────────────────────── parse_large_files ──────────────────────────────
class TestParseLargeFiles:
    def test_extracts_key_and_size_pairs(self):
        resp = {"Contents": [
            {"Key": "bench/source/lg_1", "Size": 200_000_000},
            {"Key": "bench/source/lg_2", "Size": 350_000_000},
        ]}
        assert parse_large_files(resp) == [
            ("bench/source/lg_1", 200_000_000),
            ("bench/source/lg_2", 350_000_000),
        ]

    def test_empty_contents_returns_empty_list(self):
        # 空前缀（桶里没文件）不抛错，返回 []
        assert parse_large_files({"Contents": []}) == []

    def test_missing_contents_key_returns_empty_list(self):
        # list_objects_v2 在 0 命中时可能完全不带 Contents 键
        assert parse_large_files({}) == []

    def test_preserves_listing_order(self):
        resp = {"Contents": [{"Key": f"k{i}", "Size": i} for i in range(5)]}
        assert parse_large_files(resp) == [(f"k{i}", i) for i in range(5)]


# ───────────────────────── build_copy_entry ───────────────────────────────
class TestBuildCopyEntry:
    def _entry(self, **over):
        kw = dict(bucket="datatos3-code", key="bench/source/lg_1",
                  size=200_000_000, run_id="1700000000", worker_id=0, seq=0)
        kw.update(over)
        return build_copy_entry(**kw)

    def test_source_maps_to_s3_remote(self):
        body = json.loads(self._entry()["MessageBody"])
        assert body["source"] == "s3:datatos3-code/bench/source/lg_1"

    def test_destination_uses_basename_not_full_key(self):
        # dest 只取源 key 的 basename，拼进唯一前缀
        body = json.loads(self._entry(key="a/b/c/file.bin")["MessageBody"])
        assert body["destination"].endswith("_file.bin")
        assert "a/b/c" not in body["destination"]

    def test_destination_embeds_run_worker_seq_for_uniqueness(self):
        body = json.loads(
            self._entry(run_id="RID", worker_id=7, seq=42, key="x/f.bin")["MessageBody"]
        )
        assert body["destination"] == "s3:datatos3-code/bench/run_RID/w7_42_f.bin"

    def test_key_without_slash_uses_whole_key_as_name(self):
        body = json.loads(self._entry(key="flatname")["MessageBody"])
        assert body["destination"].endswith("_flatname")

    def test_object_size_attribute_is_stringified_number(self):
        # worker 据 object_size 路由 large/small 队列；必须是字符串型数字
        entry = self._entry(size=350_000_000)
        attr = entry["MessageAttributes"]["object_size"]
        assert attr["DataType"] == "Number"
        assert attr["StringValue"] == "350000000"

    def test_entry_id_encodes_worker_and_seq(self):
        # SQS batch entry Id 在单批内必须唯一
        assert self._entry(worker_id=3, seq=9)["Id"] == "w3n9"

    def test_body_is_valid_json(self):
        body = self._entry()["MessageBody"]
        assert isinstance(body, str)
        json.loads(body)  # 不抛即合法

    def test_only_source_and_destination_in_body(self):
        # copy 消息体不该掺 op 字段（默认即 copy）
        body = json.loads(self._entry()["MessageBody"])
        assert set(body) == {"source", "destination"}


class TestUniqueKeyContract:
    """防 skip 核心契约：不同 (run_id, worker_id, seq) 必产生不同 destination。"""

    def _dest(self, **over):
        kw = dict(bucket="b", key="src/f.bin", size=1,
                  run_id="R", worker_id=0, seq=0)
        kw.update(over)
        return json.loads(build_copy_entry(**kw)["MessageBody"])["destination"]

    def test_distinct_seq_yields_distinct_destination(self):
        assert self._dest(seq=0) != self._dest(seq=1)

    def test_distinct_worker_yields_distinct_destination(self):
        assert self._dest(worker_id=0) != self._dest(worker_id=1)

    def test_distinct_run_id_yields_distinct_destination(self):
        # 跨 feeder 重启用新 run_id，旧 run 的目标不会被复用 → 不 skip
        assert self._dest(run_id="A") != self._dest(run_id="B")

    def test_full_grid_all_destinations_unique(self):
        dests = {
            self._dest(run_id=r, worker_id=w, seq=s)
            for r in ("R1", "R2")
            for w in range(4)
            for s in range(25)
        }
        assert len(dests) == 2 * 4 * 25  # 无碰撞

    def test_same_inputs_are_deterministic(self):
        # 纯函数：同输入同输出（无隐藏随机/时间）
        assert self._dest(run_id="R", worker_id=2, seq=5) == \
               self._dest(run_id="R", worker_id=2, seq=5)
