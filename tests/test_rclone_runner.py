"""rclone_runner 单元测试（先写，TDD RED）。

通过注入 fake runner / classifier 回调，使本测试不依赖真实 rclone 二进制，
也不依赖并行开发中的 error_classifier 模块。
"""
from __future__ import annotations

import json
import subprocess

import pytest

from migration import config
from migration.models import RunResult, State, TransferMessage, TransferStats
from migration.rclone_runner import (
    _default_runner,
    build_cmd,
    parse_json_log,
    run,
    sanitize_rclone_args,
)

# ── fake runner ───────────────────────────────────────────────────────────────


class FakeCompleted:
    """模拟 subprocess.CompletedProcess（stdout/stderr 为 bytes）。"""

    def __init__(self, returncode: int, stdout: bytes = b"", stderr: bytes = b""):
        self.returncode = returncode
        self.stdout = stdout
        self.stderr = stderr


def make_runner(*, returncode=0, stdout=b"", stderr=b"", raises=None):
    """构造一个可注入的 fake subprocess.run，记录被调用的 cmd。"""
    calls: list[list[str]] = []

    def _runner(cmd, **kwargs):
        calls.append(cmd)
        if raises is not None:
            raise raises
        return FakeCompleted(returncode, stdout, stderr)

    _runner.calls = calls  # type: ignore[attr-defined]
    return _runner


def msg(source="s3src:bucket/a.txt", destination="s3:dst/a.txt", args=(), op=None):
    kw = {"source": source, "destination": destination, "rclone_args": tuple(args)}
    if op is not None:
        kw["op"] = op
    return TransferMessage(**kw)


# ── sanitize_rclone_args ────────────────────────────────────────────────────


@pytest.mark.unit
class TestSanitize:
    def test_keeps_allowed_value_flag(self):
        out = sanitize_rclone_args(["--header-upload", "X-Foo: bar"])
        assert out == ["--header-upload", "X-Foo: bar"]

    def test_keeps_allowed_storage_class(self):
        out = sanitize_rclone_args(["--s3-storage-class", "GLACIER"])
        assert out == ["--s3-storage-class", "GLACIER"]

    def test_keeps_disable_flag(self):
        # --disable copy：压测强制 S3→S3 走下载+上传(非 server-side copy)
        out = sanitize_rclone_args(["--disable", "copy"])
        assert out == ["--disable", "copy"]

    def test_keeps_allowed_boolean_flag(self):
        # --progress 在白名单但不需要 value
        out = sanitize_rclone_args(["--progress"])
        assert out == ["--progress"]

    def test_drops_non_whitelisted_flag(self):
        # --ignore-errors 不在白名单，静默丢弃
        out = sanitize_rclone_args(["--ignore-errors"])
        assert out == []

    def test_drops_non_whitelisted_but_keeps_following_whitelisted(self):
        out = sanitize_rclone_args(["--ignore-errors", "--progress"])
        assert out == ["--progress"]

    def test_rejects_value_with_newline(self):
        out = sanitize_rclone_args(["--header-upload", "X-Foo: bar\ninjected"])
        # value 含 \n -> flag 与 value 一并丢弃
        assert out == []

    def test_rejects_value_with_carriage_return(self):
        out = sanitize_rclone_args(["--s3-storage-class", "STD\rEVIL"])
        assert out == []

    def test_rejects_value_with_null_byte(self):
        out = sanitize_rclone_args(["--header-upload", "a\x00b"])
        assert out == []

    def test_rejects_value_starting_with_double_dash(self):
        # value 以 -- 开头，疑似注入新 flag
        out = sanitize_rclone_args(["--s3-storage-class", "--evil"])
        assert out == []

    def test_value_flag_missing_value_is_dropped(self):
        # 末尾缺 value
        out = sanitize_rclone_args(["--header-upload"])
        assert out == []

    def test_skips_non_str_elements(self):
        out = sanitize_rclone_args([123, "--progress", None])  # type: ignore[list-item]
        assert out == ["--progress"]

    def test_value_consumed_not_treated_as_flag(self):
        # value "GLACIER" 不应被当作潜在 flag 再处理
        out = sanitize_rclone_args(["--s3-storage-class", "GLACIER", "--progress"])
        assert out == ["--s3-storage-class", "GLACIER", "--progress"]

    def test_empty_input(self):
        assert sanitize_rclone_args([]) == []

    def test_value_flag_with_non_str_value_dropped(self):
        # value 位置是非 str（如 int）-> flag 与 value 一并丢弃
        out = sanitize_rclone_args(["--header-upload", 123])  # type: ignore[list-item]
        assert out == []


# ── build_cmd ────────────────────────────────────────────────────────────────


@pytest.mark.unit
class TestBuildCmd:
    def test_uses_rclone_bin_and_copyto(self):
        cmd = build_cmd(msg(), "/cfg/rclone.conf", is_large=False)
        assert cmd[0] == config.RCLONE_BIN
        assert cmd[1] == "copyto"

    def test_config_path_present(self):
        cmd = build_cmd(msg(), "/cfg/rclone.conf", is_large=False)
        assert "--config" in cmd
        idx = cmd.index("--config")
        assert cmd[idx + 1] == "/cfg/rclone.conf"

    def test_upload_flags_always_present_regardless_of_is_large(self):
        # 根因修复：分片不再依赖 is_large（worker 预知大小常缺失）。
        # 统一带 --s3-upload-cutoff=100M，rclone 按真实大小自动 multipart。
        for is_large in (True, False):
            cmd = build_cmd(msg(), "/cfg/rclone.conf", is_large=is_large)
            ci = cmd.index("--s3-upload-cutoff")
            assert cmd[ci + 1] == "100M"  # 100MB 阈值，> 它的对象自动多分片
            for flag in ("--s3-chunk-size", "--s3-upload-concurrency"):
                assert flag in cmd
            bi = cmd.index("--buffer-size")
            assert cmd[bi + 1] == "32M"
            mi = cmd.index("--max-buffer-memory")
            assert cmd[mi + 1] == "2G"

    def test_common_flags_present(self):
        cmd = build_cmd(msg(), "/cfg/rclone.conf", is_large=False)
        for flag in (
            "--s3-no-check-bucket",
            "--s3-disable-checksum",
            "--s3-no-head",
            "--use-mmap",
            "--no-traverse",
            "--use-json-log",
        ):
            assert flag in cmd

    def test_metadata_flag_present(self):
        # 客户要求把 GCS 侧 metadata（content-type/cache-control/mtime/X-Goog-Meta-* 等）
        # 照搬到 S3。rclone --metadata 默认关闭，必须显式开；copy/delete 都应带上。
        copy_cmd = build_cmd(msg(), "/cfg/rclone.conf", is_large=False)
        assert "--metadata" in copy_cmd

    def test_stats_interval_is_periodic_not_zero(self):
        # H3 根因：--stats 0 关闭周期输出 → 单文件全程无 stats 行 →
        # speed/elapsed 恒为 0。必须是有限周期，让 rclone 周期输出 stats 行。
        cmd = build_cmd(msg(), "/cfg/rclone.conf", is_large=False)
        assert "--stats" in cmd
        si = cmd.index("--stats")
        assert cmd[si + 1] != "0"
        # 形如 "30s"：含时间单位，非纯 "0"
        assert cmd[si + 1].endswith("s")

    def test_terminator_before_positionals(self):
        cmd = build_cmd(msg(), "/cfg/rclone.conf", is_large=False)
        # 最后三个元素应是: -- source destination
        assert cmd[-3] == "--"
        assert cmd[-2] == "s3src:bucket/a.txt"
        assert cmd[-1] == "s3:dst/a.txt"

    def test_double_dash_terminator_exists(self):
        cmd = build_cmd(msg(), "/cfg/rclone.conf", is_large=False)
        assert "--" in cmd

    def test_sanitized_args_before_terminator(self):
        m = msg(args=["--s3-storage-class", "GLACIER", "--ignore-errors"])
        cmd = build_cmd(m, "/cfg/rclone.conf", is_large=False)
        term = cmd.index("--")
        before = cmd[:term]
        assert "--s3-storage-class" in before
        assert "GLACIER" in before
        # 被过滤掉
        assert "--ignore-errors" not in cmd

    def test_bwlimit_default_off_omits_flag(self):
        # 默认 bwlimit="off" → 不拼 --bwlimit（rclone 默认不限速，少一个 flag）。
        cmd = build_cmd(msg(), "/cfg/rclone.conf", is_large=False)
        assert "--bwlimit" not in cmd

    def test_bwlimit_value_injected_before_terminator(self):
        # 指定限速值 → --bwlimit <值> 出现在 -- 终结符之前（属 flag 区）。
        cmd = build_cmd(msg(), "/cfg/rclone.conf", is_large=False, bwlimit="480000000")
        assert "--bwlimit" in cmd
        bi = cmd.index("--bwlimit")
        assert cmd[bi + 1] == "480000000"
        assert bi < cmd.index("--"), "--bwlimit 必须在终结符前"

    def test_bwlimit_off_string_omits_flag(self):
        # 显式传 "off" 等同默认，不拼 flag。
        cmd = build_cmd(msg(), "/cfg/rclone.conf", is_large=False, bwlimit="off")
        assert "--bwlimit" not in cmd

    # ── --tpslimit（请求/秒限速，与 bwlimit 对称；手动 SSM 值，不接 AIMD）──
    def test_tpslimit_default_off_omits_flag(self):
        # 默认 tpslimit="off" → 不拼 --tpslimit（rclone 默认不限请求频率）。
        cmd = build_cmd(msg(), "/cfg/rclone.conf", is_large=False)
        assert "--tpslimit" not in cmd

    def test_tpslimit_value_injected_before_terminator(self):
        # 指定值 → --tpslimit <值> 出现在 -- 终结符之前（属 flag 区）。
        cmd = build_cmd(msg(), "/cfg/rclone.conf", is_large=False, tpslimit="4000")
        assert "--tpslimit" in cmd
        ti = cmd.index("--tpslimit")
        assert cmd[ti + 1] == "4000"
        assert ti < cmd.index("--"), "--tpslimit 必须在终结符前"

    def test_tpslimit_off_string_omits_flag(self):
        cmd = build_cmd(msg(), "/cfg/rclone.conf", is_large=False, tpslimit="off")
        assert "--tpslimit" not in cmd

    def test_bwlimit_and_tpslimit_coexist(self):
        # 两维限速可同时下发，互不影响。
        cmd = build_cmd(
            msg(), "/cfg/rclone.conf", is_large=False,
            bwlimit="480000000", tpslimit="4000",
        )
        assert "--bwlimit" in cmd
        assert "--tpslimit" in cmd

    # ── op=delete：rclone deletefile destination ──
    def test_delete_op_uses_deletefile_subcommand(self):
        from migration.models import Op

        m = msg(op=Op.DELETE)
        cmd = build_cmd(m, "/cfg/rclone.conf", is_large=False)
        assert cmd[0] == config.RCLONE_BIN
        assert cmd[1] == "deletefile"
        # deletefile 只删目标端单对象（destination 在终结符后）
        assert cmd[-2] == "--"
        assert cmd[-1] == m.destination

    def test_delete_op_omits_transfer_flags(self):
        # 删除不传输 → 不拼 size flags / bwlimit / copyto 专用调优。
        from migration.models import Op

        cmd = build_cmd(
            msg(op=Op.DELETE), "/cfg/rclone.conf", is_large=True,
            bwlimit="480000000", tpslimit="4000",
        )
        assert "copyto" not in cmd
        assert "--bwlimit" not in cmd
        assert "--tpslimit" not in cmd
        assert "--s3-upload-cutoff" not in cmd
        assert "--s3-chunk-size" not in cmd
        # 仍带 --config
        assert "--config" in cmd

    def test_delete_op_validates_destination(self):
        # 仍走 destination 端点校验（缺 backend 前缀报错）
        from migration.models import Op

        with pytest.raises(ValueError):
            build_cmd(
                msg(op=Op.DELETE, destination="dst/a.txt"),
                "/cfg/rclone.conf", is_large=False,
            )

    def test_source_without_backend_prefix_raises(self):
        with pytest.raises(ValueError):
            build_cmd(msg(source="bucket/a.txt"), "/cfg/rclone.conf", is_large=False)

    def test_dest_without_backend_prefix_raises(self):
        with pytest.raises(ValueError):
            build_cmd(msg(destination="dst/a.txt"), "/cfg/rclone.conf", is_large=False)

    def test_source_with_null_byte_raises(self):
        with pytest.raises(ValueError):
            build_cmd(msg(source="s3src:a\x00b"), "/cfg/rclone.conf", is_large=False)

    def test_dest_with_null_byte_raises(self):
        with pytest.raises(ValueError):
            build_cmd(msg(destination="s3:a\x00b"), "/cfg/rclone.conf", is_large=False)

    def test_prefix_must_be_alnum_before_colon(self):
        # 冒号前不是字母数字（以 - 开头）应被拒
        with pytest.raises(ValueError):
            build_cmd(msg(source="-bad:x"), "/cfg/rclone.conf", is_large=False)

    def test_no_shell_injection_chars_leak(self):
        # 含 shell 元字符的 source 不抛错（不走 shell），原样作为 argv
        m = msg(source="s3src:a;rm -rf/.txt")
        cmd = build_cmd(m, "/cfg/rclone.conf", is_large=False)
        assert cmd[-2] == "s3src:a;rm -rf/.txt"


# ── parse_json_log ───────────────────────────────────────────────────────────


@pytest.mark.unit
class TestParseJsonLog:
    def test_extracts_last_stats_line(self):
        lines = [
            json.dumps({"level": "info", "msg": "starting"}),
            json.dumps(
                {
                    "stats": {
                        "bytes": 100,
                        "elapsedTime": 1.5,
                        "speed": 66.6,
                        "errors": 0,
                        "transfers": 1,
                    }
                }
            ),
            json.dumps(
                {
                    "stats": {
                        "bytes": 200,
                        "elapsedTime": 3.0,
                        "speed": 66.6,
                        "errors": 0,
                        "transfers": 2,
                    }
                }
            ),
        ]
        stats = parse_json_log("\n".join(lines))
        assert stats.bytes == 200
        assert stats.transfers == 2
        assert stats.elapsed_seconds == 3.0

    def test_no_stats_line_returns_empty(self):
        stats = parse_json_log(json.dumps({"level": "info", "msg": "hi"}))
        assert stats == TransferStats()

    def test_empty_string_returns_empty(self):
        assert parse_json_log("") == TransferStats()

    def test_tolerates_non_json_lines(self):
        text = "\n".join(
            [
                "not json at all {{{",
                "",
                json.dumps(
                    {"stats": {"bytes": 50, "elapsedTime": 1.0, "speed": 50.0,
                               "errors": 1, "transfers": 1}}
                ),
                "   ",
                "trailing garbage",
            ]
        )
        stats = parse_json_log(text)
        assert stats.bytes == 50
        assert stats.errors == 1

    def test_tolerates_special_chars_in_lines(self):
        text = "\n".join(
            [
                json.dumps({"msg": "emoji 🚀 and unicode ✓ \\ backslash"}),
                json.dumps(
                    {"stats": {"bytes": 7, "elapsedTime": 0.1, "speed": 70.0,
                               "errors": 0, "transfers": 1}}
                ),
            ]
        )
        stats = parse_json_log(text)
        assert stats.bytes == 7

    def test_stats_missing_fields_default_zero(self):
        text = json.dumps({"stats": {"bytes": 10}})
        stats = parse_json_log(text)
        assert stats.bytes == 10
        assert stats.errors == 0
        assert stats.transfers == 0
        assert stats.speed == 0.0

    def test_stats_not_a_dict_ignored(self):
        text = json.dumps({"stats": "oops"})
        # "stats" 字段存在但不是 dict -> 容错返回空
        assert parse_json_log(text) == TransferStats()


# ── H2: 进程组管理（_default_runner）────────────────────────────────────────


class FakePopen:
    """模拟 subprocess.Popen，记录构造 kwargs，可注入 communicate 行为。"""

    instances: list[FakePopen] = []

    def __init__(self, cmd, **kwargs):
        self.cmd = cmd
        self.kwargs = kwargs
        self.pid = 4242
        self.returncode = None
        self._communicate_raises = None
        self._stdout = b""
        self._stderr = b""
        self.kill_called = False
        self.wait_called = False
        FakePopen.instances.append(self)

    def communicate(self, timeout=None):
        self.timeout_used = timeout
        if self._communicate_raises is not None:
            raise self._communicate_raises
        self.returncode = 0
        return self._stdout, self._stderr

    def kill(self):
        self.kill_called = True

    def wait(self, timeout=None):
        self.wait_called = True
        self.returncode = -9
        return self.returncode


@pytest.fixture(autouse=False)
def _clear_popen():
    FakePopen.instances.clear()
    yield
    FakePopen.instances.clear()


@pytest.mark.unit
class TestDefaultRunnerProcessGroup:
    def test_uses_new_session_for_process_group(self, _clear_popen):
        # rclone 子进程必须独立进程组（start_new_session=True），
        # 才能在 timeout/shutdown 时整组收尾，不留孤儿继续写 S3。
        def popen_factory(cmd, **kwargs):
            return FakePopen(cmd, **kwargs)

        completed = _default_runner(
            ["rclone", "copyto"],
            capture_output=True,
            check=False,
            timeout=100,
            popen_factory=popen_factory,
        )
        assert FakePopen.instances[0].kwargs.get("start_new_session") is True
        assert completed.returncode == 0

    def test_returns_completed_like_object(self, _clear_popen):
        # 必须保持 RunResult 上游契约：返回对象有 returncode/stdout/stderr
        def popen_factory(cmd, **kwargs):
            p = FakePopen(cmd, **kwargs)
            p._stdout = b"out"
            p._stderr = b"err"
            return p

        completed = _default_runner(
            ["rclone"], capture_output=True, check=False, timeout=100,
            popen_factory=popen_factory,
        )
        assert completed.returncode == 0
        assert completed.stdout == b"out"
        assert completed.stderr == b"err"

    def test_timeout_kills_whole_process_group(self, _clear_popen, monkeypatch):
        # communicate 超时 → 必须对整个进程组发 SIGKILL（os.killpg），
        # 而非只杀主进程（否则 rclone multipart 子操作变孤儿继续写 S3）。
        killed = {}

        def fake_killpg(pgid, sig):
            killed["pgid"] = pgid
            killed["sig"] = sig

        monkeypatch.setattr("migration.rclone_runner.os.killpg", fake_killpg)
        monkeypatch.setattr("migration.rclone_runner.os.getpgid", lambda pid: pid)

        def popen_factory(cmd, **kwargs):
            p = FakePopen(cmd, **kwargs)
            p._communicate_raises = subprocess.TimeoutExpired(cmd="rclone", timeout=1)
            return p

        with pytest.raises(subprocess.TimeoutExpired):
            _default_runner(
                ["rclone"], capture_output=True, check=False, timeout=1,
                popen_factory=popen_factory,
            )
        # 杀的是进程组（pgid = pid），信号为 SIGKILL
        import signal as _signal
        assert killed["pgid"] == 4242
        assert killed["sig"] == _signal.SIGKILL


@pytest.mark.unit
class TestRunStillRoutesThroughDefaultRunner:
    def test_timeout_via_default_runner_returns_unknown(self, monkeypatch):
        # run() 默认 runner=_default_runner；超时仍归类 UNKNOWN/rclone_timeout，
        # 保持原 RunResult 契约不变。
        def boom_popen(cmd, **kwargs):
            p = FakePopen(cmd, **kwargs)
            p._communicate_raises = subprocess.TimeoutExpired(cmd="rclone", timeout=1)
            return p

        monkeypatch.setattr("migration.rclone_runner.os.killpg", lambda *a: None)
        monkeypatch.setattr("migration.rclone_runner.os.getpgid", lambda pid: pid)

        def runner(cmd, **kwargs):
            return _default_runner(cmd, popen_factory=boom_popen, **kwargs)

        res = run(
            msg(), "/cfg/rclone.conf", is_large=False,
            runner=runner, classify_state=fake_state, classify_error=fake_err,
        )
        assert res.state is State.UNKNOWN
        assert res.error_class == "rclone_timeout"


# ── run ──────────────────────────────────────────────────────────────────────


def fake_state(code: int) -> State:
    return State.SUCCESS if code == 0 else State.RETRYABLE


def fake_err(code: int, stderr: str) -> str | None:
    return None if code == 0 else "some_error"


@pytest.mark.unit
class TestRun:
    def _stats_stderr(self) -> bytes:
        return json.dumps(
            {"stats": {"bytes": 100, "elapsedTime": 1.0, "speed": 100.0,
                       "errors": 0, "transfers": 1}}
        ).encode()

    def test_success(self):
        runner = make_runner(returncode=0, stderr=self._stats_stderr())
        res = run(
            msg(), "/cfg/rclone.conf", is_large=False,
            runner=runner, classify_state=fake_state, classify_error=fake_err,
        )
        assert isinstance(res, RunResult)
        assert res.state is State.SUCCESS
        assert res.exit_code == 0
        assert res.error_class is None
        assert res.stats.bytes == 100
        assert res.cmd_str  # shlex.join 非空

    def test_retryable(self):
        runner = make_runner(returncode=1, stderr=b"some failure")
        res = run(
            msg(), "/cfg/rclone.conf", is_large=False,
            runner=runner, classify_state=fake_state, classify_error=fake_err,
        )
        assert res.state is State.RETRYABLE
        assert res.exit_code == 1
        assert res.error_class == "some_error"

    def test_fatal(self):
        def state(code):
            return State.FATAL

        runner = make_runner(returncode=3, stderr=b"directory not found")
        res = run(
            msg(), "/cfg/rclone.conf", is_large=False,
            runner=runner, classify_state=state,
            classify_error=lambda c, s: "dir_not_found",
        )
        assert res.state is State.FATAL
        assert res.error_class == "dir_not_found"

    def test_unknown_state(self):
        def state(code):
            return State.UNKNOWN

        runner = make_runner(returncode=137, stderr=b"killed")
        res = run(
            msg(), "/cfg/rclone.conf", is_large=False,
            runner=runner, classify_state=state, classify_error=fake_err,
        )
        assert res.state is State.UNKNOWN

    def test_gcs_429_upgraded_unknown_to_retryable(self):
        # 根因修复：源端 429（egress bandwidth 配额）→ rclone 退出码 1 →
        # 真实 classify_state(1)=UNKNOWN，但 stderr 命中瞬时错误 → 升级为 RETRYABLE。
        # 用真实分类器（不注入），验证 run() 内的升级逻辑。
        stderr = (
            b'{"level":"error","msg":"Failed to copy: multi-thread copy: '
            b'failed to open source: googleapi: got HTTP response code 429 '
            b'with body: This workload is drawing too much egress bandwidth '
            b'from Cloud Storage and has exceeded the InternetEgressBandwidth quota"}'
        )
        runner = make_runner(returncode=1, stderr=stderr)
        res = run(msg(), "/cfg/rclone.conf", is_large=False, runner=runner)
        assert res.state is State.RETRYABLE
        assert res.exit_code == 1
        assert res.error_class == "src_rate_limit"

    def test_source_missing_upgraded_unknown_to_fatal(self):
        # 根因修复：源对象不存在时 rclone copyto 走 cmd/cmd.go 的 generic critical
        # 路径以退出码 1 退出（不是 3/4）→ classify_state(1)=UNKNOWN → 不删不计数、
        # 空占 12h visibility × 3 次（36h）才进 DLQ。但"源不存在"是确定性终态，
        # 重试永远不会成功，应升级 FATAL（删消息 + 计数 + DDB 记 src_not_found）。
        # stderr 为 2026-06-10 生产真实日志。用真实分类器验证 run() 内的升级逻辑。
        stderr = (
            b'{"time":"2026-06-10T05:37:09.549882929Z","level":"info",'
            b'"msg":"Starting bandwidth limiter at 9.540Mi Byte/s",'
            b'"source":"accounting/token_bucket.go:109"}\n'
            b'{"time":"2026-06-10T05:37:09.722089965Z","level":"critical",'
            b'"msg":"Source doesn\'t exist or is a directory and destination is a file",'
            b'"source":"cmd/cmd.go:204"}'
        )
        runner = make_runner(returncode=1, stderr=stderr)
        res = run(msg(), "/cfg/rclone.conf", is_large=False, runner=runner)
        assert res.state is State.FATAL
        assert res.exit_code == 1
        assert res.error_class == "src_not_found"

    def test_real_crash_stays_unknown(self):
        # 对照：真正的崩溃（SIGKILL，退出码 137，stderr 无瞬时关键词）仍保持 UNKNOWN，
        # 不被误升级。用真实分类器。
        runner = make_runner(returncode=137, stderr=b'{"level":"error","msg":"signal: killed"}')
        res = run(msg(), "/cfg/rclone.conf", is_large=False, runner=runner)
        assert res.state is State.UNKNOWN

    def test_timeout(self):
        runner = make_runner(
            raises=subprocess.TimeoutExpired(cmd="rclone", timeout=10)
        )
        res = run(
            msg(), "/cfg/rclone.conf", is_large=False,
            runner=runner, classify_state=fake_state, classify_error=fake_err,
        )
        assert res.state is State.UNKNOWN
        assert res.exit_code == -1
        assert res.error_class == "rclone_timeout"

    def test_oserror_becomes_subprocess_error(self):
        runner = make_runner(raises=OSError("boom"))
        res = run(
            msg(), "/cfg/rclone.conf", is_large=False,
            runner=runner, classify_state=fake_state, classify_error=fake_err,
        )
        assert res.state is State.UNKNOWN
        assert res.error_class == "subprocess_error"

    def test_value_error_from_build_becomes_subprocess_error(self):
        # source 缺前缀 -> build_cmd 抛 ValueError -> run 捕获
        runner = make_runner(returncode=0)
        res = run(
            msg(source="bucket/no-prefix.txt"), "/cfg/rclone.conf", is_large=False,
            runner=runner, classify_state=fake_state, classify_error=fake_err,
        )
        assert res.state is State.UNKNOWN
        assert res.error_class == "subprocess_error"

    def test_stderr_decoded_backslashreplace(self):
        # 非法 UTF-8 字节序列不应崩溃
        runner = make_runner(returncode=1, stderr=b"\xff\xfe bad bytes")
        res = run(
            msg(), "/cfg/rclone.conf", is_large=False,
            runner=runner, classify_state=fake_state, classify_error=fake_err,
        )
        assert res.state is State.RETRYABLE
        assert res.error_message is not None

    def test_runner_called_with_capture_and_no_shell(self):
        captured = {}

        def runner(cmd, **kwargs):
            captured.update(kwargs)
            captured["cmd"] = cmd
            return FakeCompleted(0, stderr=self._stats_stderr())

        run(
            msg(), "/cfg/rclone.conf", is_large=False,
            runner=runner, classify_state=fake_state, classify_error=fake_err,
        )
        assert captured.get("capture_output") is True
        assert captured.get("check") is False
        assert captured.get("timeout") == config.RCLONE_TIMEOUT_SECONDS
        assert "shell" not in captured or captured["shell"] is False
        assert isinstance(captured["cmd"], list)

    def test_default_classifiers_lazy_import(self):
        # 不传 classify_* 时应延迟 import error_classifier 模块；
        # 若该模块尚未就绪则跳过此用例（并行开发约定）。
        pytest.importorskip("migration.error_classifier")
        runner = make_runner(returncode=0, stderr=self._stats_stderr())
        res = run(msg(), "/cfg/rclone.conf", is_large=False, runner=runner)
        assert isinstance(res, RunResult)


def test_parse_json_log_fallback_to_copied_size():
    """无 stats 行时，从 'Copied' 行的 size 累加字节（fallback 路径）。

    注意：H3 修复后（--stats 30s）单文件传输也会输出周期 stats 行，正常走
    主分支；此用例仅覆盖 fallback 路径的健壮性（speed/elapsed 取不到时为 0，
    但 bytes 必须准确，不能丢字节）。
    """
    stderr = (
        '{"level":"info","msg":"Copied (new) to: a.bin","size":10295,"object":"a.bin"}\n'
    )
    stats = parse_json_log(stderr)
    assert stats.bytes == 10295
    assert stats.transfers == 1
    # fallback 路径拿不到 speed/elapsed，明确为 0（不是 bug，是已知降级）
    assert stats.speed == 0.0
    assert stats.elapsed_seconds == 0.0


def test_parse_json_log_single_file_with_periodic_stats_has_speed_elapsed():
    """H3 修复验证：--stats 30s 后单文件传输输出周期 stats 行，
    parse_json_log 走主分支拿到 speed/elapsed（不再恒为 0）。

    模拟修复后真实 NDJSON：一条 Copied 行 + 一条周期 stats 行。
    """
    stderr = "\n".join(
        [
            '{"level":"info","msg":"Copied (new) to: a.bin","size":10295,"object":"a.bin"}',
            '{"level":"info","stats":{"bytes":10295,"elapsedTime":2.5,'
            '"speed":4118.0,"errors":0,"transfers":1}}',
        ]
    )
    stats = parse_json_log(stderr)
    assert stats.bytes == 10295
    assert stats.transfers == 1
    # 关键：speed/elapsed 不再是 0（这是 H3 修复要暴露并验证的核心）
    assert stats.speed == 4118.0
    assert stats.elapsed_seconds == 2.5


def test_parse_json_log_stats_line_takes_priority():
    """有 stats 行时优先用 stats，不走 Copied fallback。"""
    stderr = (
        '{"msg":"Copied (new) to: a.bin","size":10295}\n'
        '{"stats":{"bytes":99999,"elapsedTime":1.5,"speed":66666,"errors":0,"transfers":1}}\n'
    )
    stats = parse_json_log(stderr)
    assert stats.bytes == 99999


def test_parse_json_log_multithread_copied():
    """大文件 multi-thread 传输：msg='Multi-thread Copied ...'，也要抓到 size。"""
    stderr = '{"level":"info","msg":"Multi-thread Copied (new) to: lg.bin","size":524288000}\n'
    stats = parse_json_log(stderr)
    assert stats.bytes == 524288000
    assert stats.transfers == 1
