# MCWarden

Minecraftサーバーの運用・監視・診断を行う、Go製CLIの開発初期段階です。

現在はRuntime / Minecraft Controlの境界、停止・再起動の制御、部分的なstatus取得、JSON出力とテストを実装しています。Linux向けの`DirectProcess`アダプター（`internal/runtime/direct`）とTOML設定・server selection、MCWarden管理下プロセスのidentity永続化を実装し、`status`・`start`・`state`はCLIから利用できます。Minecraft側のcontrol/readiness adapterはまだ無いため、`start`（既定）・`stop`・`restart`は終了コード5を返します。`start --no-wait`はプロセス起動後に返ります。`kill`のCLI配線はまだです。

```sh
go test -race ./...
go build -o bin/mcwarden ./cmd/mcwarden
./bin/mcwarden help
```

Go 1.22以降が必要です。TOMLの読み込みに`github.com/BurntSushi/toml`を使用します。

## 設定

既定の設定ファイルは`${XDG_CONFIG_HOME:-~/.config}/mcwarden/config.toml`です。`-c/--config`で上書きできます。

```toml
default_server = "rail"

[servers.rail]
runtime = "direct"

[servers.rail.direct]
command = "/srv/minecraft/run.sh" # JVM本体、または最後に exec java する wrapper
args = ["--nogui"]
dir = "/srv/minecraft"
env = ["JAVA_HOME=/usr/lib/jvm/temurin-21"]

# 既存プロセスを採用する場合（任意）。PID または "PID STARTTIME"。
# pidfile = "/run/mcwarden/rail.pid"
# signal_group = false
```

`-s/--server`でサーバーを選びます。省略時は`default_server`、それが無ければ唯一のサーバーを使います。グローバルオプションはコマンドの前後どちらでも置けます。

```sh
mcwarden -c ./config.toml -s rail status
mcwarden status -s rail --json
```

## process identity state

```sh
mcwarden -s rail start --no-wait     # プロセス起動後に返る
mcwarden -s rail status              # 別invocationから同じ世代を観測
mcwarden -s rail state show          # absent / managed / stale / unknown とidentity
mcwarden -s rail state clean         # staleだけを明示的に削除
```

MCWardenが起動・採用した世代のPID・PGID・starttime・boot_idは、`${XDG_STATE_HOME:-~/.local/state}/mcwarden/<server>.json`へatomicに保存します。ログインセッションをまたいでidentityを保持するため、`XDG_RUNTIME_DIR`ではなく永続stateディレクトリを使います。ディレクトリは0700、stateは0600です。

次回のinvocationは`managed`な保存済みidentityだけを`DirectRuntime`へ復元し、`status`・`stop`・`restart`・signalが同じ世代を対象にできるようにします。復元できるstateがある場合は`direct.pidfile`より優先します。復元されたプロセスはMCWarden管理下（`owned`）ですが今回のCLIプロセスのchildではないため、終了コードは取得できません。

`managed`はliveなプロセスのidentityが一致、`stale`はPID消滅・starttime/PGID/boot_id不一致、`unknown`はprocfsやboot_idを読めない状態です。`unknown`はrecordを残して`state show`と`status`（`unknown`）だけを許し、`start`・`stop`・`restart`・signalは`ErrStateUnknown`で拒否します。自動削除も二重起動も`state clean`も行いません。`stale`は`state clean`待ちで、pidfile discoveryへfallbackしません。`direct.pidfile`は外部プロセス発見用で、この内部stateとは別物です。

`Wait`が世代の終了を確認した場合、その操作の中でstale stateを削除してから次へ進みます（`restart`の旧state削除→新state保存も同じ）。MCWarden外で終了したプロセスのstateは`state clean`で明示的に削除します。

server単位のflockは`start`・`stop`・`restart`・`kill`・`state clean`の全体を直列化します。lockファイルは`${XDG_RUNTIME_DIR}/mcwarden/<server>.lock`に置き、`XDG_RUNTIME_DIR`が無い場合は、存在し現在UID所有でgroup/other権限の無い`/run/user/<uid>`だけを使います。どちらも安全でなければエラーとなり、`/tmp`には書きません。`restart`はstop→待機/escalation→旧state削除→start→新state保存まで同じlockを保持します。

シグナル送信の直前にPID・starttime・PGIDを再検証し、不一致なら送信しません。group signalはPIDがそのgroup leaderである場合だけ行います。

停止はMinecraftの`stop`だけを使用します。再起動時のシグナル送信は`--term`または`--kill --force`で明示した場合に限定し、停止確認後にだけ起動します。再起動はMinecraftの起動完了まで待ち、`--no-wait`ではプロセス起動後に返します。

[設計](docs/design.md) / [MIT License](LICENSE)
