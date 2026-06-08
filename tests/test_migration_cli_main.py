"""Tests for migration_cli.main() dispatch (monkeypatched clients + env)."""
import pytest

from migration import migration_cli as cli


@pytest.fixture
def _env(monkeypatch):
    monkeypatch.setenv("AWS_REGION", "eu-central-1")
    monkeypatch.setenv("QUEUE_URL", "http://queue")


def test_main_inspect(_env, monkeypatch, capsys):
    monkeypatch.setattr(cli, "get_dynamodb_client", lambda region: object())
    monkeypatch.setattr(cli, "cmd_inspect", lambda c, t, s: [{"state": "SUCCESS"}])
    rc = cli.main(["inspect", "s3src:a/x"])
    assert rc == 0
    assert "SUCCESS" in capsys.readouterr().out


def test_main_replay(_env, monkeypatch, capsys):
    captured = {}
    monkeypatch.setattr(cli, "get_sqs_client", lambda region: object())
    # HIGH-4: DLQ URL 由运行时解析得到，不再字符串拼接
    monkeypatch.setattr(
        cli, "resolve_dlq_url",
        lambda sqs, main_url: f"resolved-dlq-for::{main_url}",
    )

    def fake_replay(sqs, *, dlq_url, main_url, max_messages):
        captured.update(dlq_url=dlq_url, main_url=main_url, max=max_messages)
        return 7

    monkeypatch.setattr(cli, "cmd_replay", fake_replay)
    rc = cli.main(["replay", "--max", "7"])
    assert rc == 0
    # 单队列：replay 始终回唯一主队列
    assert captured["main_url"] == "http://queue"
    assert captured["dlq_url"] == "resolved-dlq-for::http://queue"
    assert captured["max"] == 7
    assert "7" in capsys.readouterr().out


def test_main_active_workers(_env, monkeypatch, capsys):
    monkeypatch.setattr(cli, "get_dynamodb_client", lambda region: object())
    monkeypatch.setattr(cli, "cmd_active_workers", lambda c, t, *, now_epoch: 9)
    rc = cli.main(["active-workers"])
    assert rc == 0
    assert "9" in capsys.readouterr().out
