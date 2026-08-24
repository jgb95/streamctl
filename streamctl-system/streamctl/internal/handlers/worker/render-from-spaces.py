#!/usr/bin/env python3
"""Download a conf-render manifest's inputs, render, and upload its outputs."""

from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path, PurePosixPath


def fail(message: str) -> None:
    raise SystemExit(message)


def run(*args: str, capture: bool = False) -> str:
    print("+ " + " ".join(args), file=sys.stderr)
    result = subprocess.run(args, check=True, text=True, capture_output=capture)
    return result.stdout if capture else ""


def remote_path(remote: str, key: str) -> str:
    return remote + key if remote.endswith((":", "/")) else remote + "/" + key


def valid_key(value: object) -> str:
    if not isinstance(value, str) or not value or value != value.strip():
        fail(f"invalid source object key: {value!r}")
    key = PurePosixPath(value)
    if key.is_absolute() or ".." in key.parts or len(key.parts) < 2 or str(key) != value:
        fail(f"source must be a relative <conference>/... object key: {value!r}")
    return value


def source_fields(manifest: dict) -> list[tuple[str, bool]]:
    fields: list[tuple[str, bool]] = []
    for job in manifest.get("jobs", []):
        for segment in job.get("segments", []):
            fields.append((valid_key(segment.get("src")), segment.get("type") == "chunkedVideo"))
            if segment.get("overlay") is not None:
                fields.append((valid_key(segment["overlay"]), False))
            audio = segment.get("audio")
            if isinstance(audio, dict) and audio.get("src") is not None:
                fields.append((valid_key(audio["src"]), False))
    if not fields:
        fail("manifest contains no source object keys")
    conferences = {PurePosixPath(key).parts[0] for key, _ in fields}
    if len(conferences) != 1:
        fail("all source object keys must belong to one conference")
    return fields


def download(remote: str, inputs: Path, key: str) -> None:
    destination = inputs / key
    destination.parent.mkdir(parents=True, exist_ok=True)
    run("rclone", "copyto", "--stats", "30s", "--stats-one-line", remote_path(remote, key), str(destination))


def download_chunks(remote: str, inputs: Path, key: str) -> None:
    source = PurePosixPath(key)
    match = re.fullmatch(r"(.*?)(\d+)(\.[^/]*)", source.name)
    if match is None:
        fail(f"chunkedVideo source must end in a numeric sequence: {key}")
    prefix, _, suffix = match.groups()
    parent = "" if str(source.parent) == "." else str(source.parent)
    listing = run("rclone", "lsf", "--files-only", remote_path(remote, parent + "/"), capture=True)
    names = sorted(name for name in listing.splitlines() if re.fullmatch(re.escape(prefix) + r"\d+" + re.escape(suffix), name))
    if source.name not in names:
        fail(f"chunkedVideo source not found: {key}")
    for name in names[names.index(source.name):]:
        download(remote, inputs, str(source.parent / name))


def localize_manifest(manifest: dict, inputs: Path) -> None:
    for job in manifest["jobs"]:
        for segment in job["segments"]:
            segment["src"] = str((inputs / segment["src"]).resolve())
            if segment.get("overlay") is not None:
                segment["overlay"] = str((inputs / segment["overlay"]).resolve())
            audio = segment.get("audio")
            if isinstance(audio, dict) and audio.get("src") is not None:
                audio["src"] = str((inputs / audio["src"]).resolve())


def transcription_enabled(job: dict) -> bool:
    return any(segment.get("transcribe") is True for segment in job.get("segments", []))


def job_outputs(job: dict) -> dict:
    job_id = job["id"]
    outputs: dict[str, object] = {
        "id": job_id,
        "video": f"{job_id}.mp4",
        "manifest": f"{job_id}.manifest.json",
        "subtitles": None,
    }
    if transcription_enabled(job):
        outputs["subtitles"] = {
            "readable": f"{job_id}.subs.srt",
            "words": f"{job_id}.words.srt",
        }
    return outputs


def main() -> None:
    if len(sys.argv) != 4:
        fail("usage: render-from-spaces.py <manifest.json> <output-dir> <work-dir>")
    manifest_path, output, work = map(Path, sys.argv[1:])
    remote = os.environ.get("SPACES_REMOTE", "").strip()
    if not remote:
        fail("SPACES_REMOTE is required")
    conf_render = os.environ.get("CONF_RENDER_COMMAND", "/root/conf-render/.venv/bin/conf-render")

    manifest_text = manifest_path.read_text(encoding="utf-8")
    original_manifest = json.loads(manifest_text)
    manifest = json.loads(manifest_text)
    fields = source_fields(manifest)
    conference = PurePosixPath(fields[0][0]).parts[0]
    inputs = work / "inputs"
    localized = work / "manifest.local.json"
    shutil.rmtree(work, ignore_errors=True)
    shutil.rmtree(output, ignore_errors=True)
    inputs.mkdir(parents=True)
    output.mkdir(parents=True)

    downloaded: set[str] = set()
    for key, chunked in fields:
        if key in downloaded:
            continue
        if chunked:
            download_chunks(remote, inputs, key)
        else:
            download(remote, inputs, key)
        downloaded.add(key)

    localize_manifest(manifest, inputs)
    localized.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    run(conf_render, "validate", str(localized))
    run(conf_render, "render", str(localized), "--output", str(output), "--work-dir", str(work / "conf-render"), "--overwrite")

    missing: list[str] = []
    for job in manifest["jobs"]:
        job_id = job["id"]
        expected = [f"{job_id}.mp4"]
        if transcription_enabled(job):
            expected.extend((f"{job_id}.subs.srt", f"{job_id}.words.srt"))
        missing.extend(name for name in expected if not (output / name).is_file())
    if missing:
        fail("conf-render did not produce expected output(s): " + ", ".join(missing))

    for job in original_manifest["jobs"]:
        job_manifest = {**original_manifest, "jobs": [job]}
        (output / f"{job['id']}.manifest.json").write_text(
            json.dumps(job_manifest, indent=2) + "\n", encoding="utf-8"
        )
    queue_id = output.name
    output_prefix = f"{conference}/recordings/renders/{queue_id}"
    run("rclone", "copy", "--stats", "30s", "--stats-one-line", str(output), remote_path(remote, output_prefix + "/"))
    files = sorted(str(item.relative_to(output)) for item in output.rglob("*") if item.is_file())
    marker = work / "ready.json"
    marker.write_text(json.dumps({
        "status": "ready",
        "manifest_job_ids": [job["id"] for job in manifest["jobs"]],
        "output_prefix": output_prefix,
        "jobs": [job_outputs(job) for job in manifest["jobs"]],
        "files": files,
    }, indent=2) + "\n", encoding="utf-8")
    run("rclone", "copyto", str(marker), remote_path(remote, output_prefix + "/ready.json"))
    print(f"render: ready {output_prefix}")


if __name__ == "__main__":
    main()
