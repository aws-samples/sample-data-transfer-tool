"""Tests for bench/feed_delete.py message-construction core (task #3).

feed_delete emits op=delete messages so the worker runs `rclone deletefile`
against the destination. Pure logic under test: delete-message body shape
(op + destination mapping) and the object_size=0 routing convention.
"""
import json

import pytest

from bench.feed_delete import build_delete_entry

pytestmark = pytest.mark.unit


class TestBuildDeleteEntry:
    def _entry(self, **over):
        kw = dict(bucket="datatos3-code", key="bench/migrated/x.bin",
                  batch_start=0, idx=0)
        kw.update(over)
        return build_delete_entry(**kw)

    def test_body_has_op_delete(self):
        body = json.loads(self._entry()["MessageBody"])
        assert body["op"] == "delete"

    def test_destination_maps_to_s3_remote(self):
        body = json.loads(self._entry(key="bench/migrated/x.bin")["MessageBody"])
        assert body["destination"] == "s3:datatos3-code/bench/migrated/x.bin"

    def test_delete_body_has_no_source(self):
        # delete 只需 destination；带 source 会让 worker 误判为 copy 校验
        body = json.loads(self._entry()["MessageBody"])
        assert "source" not in body
        assert set(body) == {"op", "destination"}

    def test_object_size_is_zero_string(self):
        # delete 不传输，size 给 "0"（路由 large/small 对 delete 无影响）
        attr = self._entry()["MessageAttributes"]["object_size"]
        assert attr["DataType"] == "Number"
        assert attr["StringValue"] == "0"

    def test_entry_id_encodes_batch_and_index(self):
        # SQS batch Id 单批内唯一：batch_start + idx
        assert self._entry(batch_start=20, idx=3)["Id"] == "d20_3"

    def test_body_is_valid_json(self):
        json.loads(self._entry()["MessageBody"])

    def test_key_with_unicode_and_spaces_preserved(self):
        # 特殊字符 key（emoji/空格）必须原样进 destination，不被破坏
        key = "bench/migrated/报告 v2 🚀.pdf"
        body = json.loads(self._entry(key=key)["MessageBody"])
        assert body["destination"] == f"s3:datatos3-code/{key}"


class TestDeleteEntryIdUniqueness:
    def test_ids_unique_within_chunked_batches(self):
        # 模拟 main() 的 range(0,len,10) 分块：所有 Id 全局唯一
        keys = [f"bench/migrated/f{i}.bin" for i in range(35)]
        ids = []
        for start in range(0, len(keys), 10):
            for i, _ in enumerate(keys[start:start + 10]):
                ids.append(build_delete_entry(
                    bucket="b", key=keys[start + i], batch_start=start, idx=i)["Id"])
        assert len(ids) == len(set(ids)) == 35
