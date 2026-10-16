#!/bin/bash
# 起容器前把持久化目录交给容器里的 uid 65532（distroless nonroot），否则写不动数据库。
set -e

dir="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$dir/data"
chown -R 65532:65532 "$dir/data"
