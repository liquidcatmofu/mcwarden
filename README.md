# MCWarden

Minecraftサーバーの運用・監視・診断を行う、Go製CLIの開発初期段階です。

現在はRuntime / Minecraft Controlの境界、停止・再起動の制御、部分的なstatus取得、JSON出力とテストを実装しています。Linux向けの`DirectProcess`アダプター（`internal/runtime/direct`）とTOML設定・server selection、MCWarden管理下プロセスのidentity永続化、RCONによるMinecraft Control（`internal/minecraft/rcon`）を実装しています。`status`・`start --no-wait`・`stop`・`restart --no-wait`・`state`はCLIから利用できます。

Readiness Observerはまだ実装していません。RCON接続はcommand transportであり、Minecraftの起動完了やhealthの証拠ではありません。そのため既定の`start`と`restart`はreadiness unsupportedで終了コード5を返します。`kill`のCLI配線もまだです。

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

[servers.rail.minecraft]
control = "rcon"

[servers.rail.minecraft.rcon]
address = "127.0.0.1:25575" # 省略時もこの値
password_file = "/srv/minecraft/secrets/rcon-password"
```

`-s/--server`でサーバーを選びます。省略時は`default_server`、それが無ければ唯一のサーバーを使います。グローバルオプションはコマンドの前後どちらでも置けます。

```sh
mcwarden -c ./config.toml -s rail status
mcwarden status -s rail --json
```

`[servers.<name>.minecraft]`が無い場合、Minecraft Controlはunsupportedのままです。`control = "rcon"`には`[servers.<name>.minecraft.rcon]`と`password_file`が必要です。未知のcontrol名や未知のTOMLキーは設定エラーになります。

## RCON control

RCONはMinecraftへのcommand transport専用です。Runtimeがプロセスのライフサイクルを所有し、RCONはコマンド送信だけを担当します。RCON adapterにRuntimeの所有権はありません。

RCONはplaintext transportです。loopback、または信頼できる暗号化されたネットワーク内でのみ使用し、インターネットへ直接公開しないでください。

`mcwarden stop`はRCONで`stop`を送信したあと、`Runtime.Wait`でプロセス終了を待ちます。タイムアウトしても`stop`自身はSIGTERM/SIGKILLへescalateしません。通信失敗・認証失敗・呼び出し元のキャンセルもescalationの理由にしません。

`mcwarden restart --no-wait`は次の順に進みます。

```text
RCON "stop"
→ Runtime.Wait
→ 旧世代のidentityをclean
→ Runtime.Start
→ 新世代のidentityを保存
→ 戻る
```

`restart --term` / `--kill --force`の既存escalationルールはそのままです。escalationは待機タイムアウト時だけ行い、RCONの通信エラーでは行いません。

RCONの`Command`は「配送を受け付けた」ことを成功とし、コマンドの効果完了までは待ちません。`stop`ではサーバーが応答前に終了し得るため、認証成功後にcommand packetを完全にwriteできた時点で成功とします。終了確認は`Runtime.Wait`が行います。RCON接続はCLI invocationを跨いで維持せず、各`Command`でdial → authenticate → command write → closeします。

RCON接続成功をMinecraft lifecycleの`Running`証拠にはしません。`process running`と`Minecraft running`は別であり、現時点ではRCON endpointが現在のRuntime generationに属するという十分な証拠がありません。Readiness Observerは次の段階で、generation-scopedな証拠の設計から実装します。

### password file

RCON passwordはCLI引数やTOMLへ直接書かず、`password_file`から読みます。ファイルはMCWardenを実行するユーザーだけが読める権限にしてください。

- 末尾の`\n`または`\r\n`だけを除去します。空白などそれ以外の文字はtrimしません。
- 除去後に空になるpasswordはcredential errorです。`stop` / `restart --no-wait`などMinecraft Controlを必要とする操作だけが失敗し、`status`・`state show`・`start --no-wait`は妨げません。
- password fileの読み込みは各RCON `Command`実行時まで遅延します。
- passwordそのものはerror、JSON output、log、テストの失敗メッセージへ出しません。
- `server.properties`の自動解析は行いません。

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

停止はMinecraftの`stop`だけを使用します。再起動時のシグナル送信は`--term`または`--kill --force`で明示した場合に限定し、停止確認後にだけ起動します。既定の`restart`はMinecraftの起動完了まで待つ設計ですが、Readiness Observerが未実装のため現在は終了コード5を返します。`--no-wait`ではRCONによるgraceful stop後にプロセスを起動して返します。

[設計](docs/design.md) / [MIT License](LICENSE)
