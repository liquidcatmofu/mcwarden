# MCWarden

Minecraftサーバーの運用・監視・診断を行う、Go製CLIの開発初期段階です。

現在はRuntime / Minecraft Control / Readiness Observerの境界、停止・再起動の制御、部分的なstatus取得、JSON出力とテストを実装しています。Linux向けの`DirectProcess`アダプター（`internal/runtime/direct`）、Quadlet-backed Podmanアダプター（`internal/runtime/podman`）とTOML設定・server selection、Direct管理下プロセスのidentity永続化、RCONによるMinecraft Control（`internal/minecraft/rcon`）、`latest.log`によるgeneration-scoped readiness（`internal/minecraft/log`）を実装しています。`status`・`start`・`stop`・`restart`はCLIから利用できます。`state`はDirect専用です。

Readinessは現在のRuntime generationに結び付けた`latest.log`のstartup完了行で判定します。RCON接続はcommand transportであり、Minecraftの起動完了やhealthの証拠ではありません。readiness observerを設定しない場合、既定の`start`と`restart`はreadiness unsupportedで終了コード5を返します。`kill`のCLI配線はまだです。

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
readiness = "log"

[servers.rail.minecraft.rcon]
address = "127.0.0.1:25575" # 省略時もこの値
password_file = "/srv/minecraft/secrets/rcon-password"

[servers.rail.minecraft.log]
path = "/srv/minecraft/logs/latest.log"
```

`-s/--server`でサーバーを選びます。省略時は`default_server`、それが無ければ唯一のサーバーを使います。グローバルオプションはコマンドの前後どちらでも置けます。

```sh
mcwarden -c ./config.toml -s rail status
mcwarden status -s rail --json
```

`[servers.<name>.minecraft]`が無い場合、Minecraft Controlとreadinessはどちらもunsupportedのままです。`control`と`readiness`は独立した設定です。`control = "rcon"`には`[servers.<name>.minecraft.rcon]`と`password_file`、`readiness = "log"`には`[servers.<name>.minecraft.log]`と`path`が必要です。未知のcontrol名・readiness名や未知のTOMLキーは設定エラーになります。

## Quadlet-backed Podman Runtime

対象は **rootless Podman + Quadlet + user systemd** です。同じユーザーのhost上で`systemctl --user`と`podman` CLIを使用します。Podman socket/APIやDBusへ直接接続せず、MCWardenはQuadletファイルの生成・編集、image build/pull/updateも行いません。事前にQuadletとuser unitを構成してください。

```toml
default_server = "rail"

[servers.rail]
runtime = "podman"

[servers.rail.podman]
unit = "minecraft-rail.service"
container = "minecraft-rail"

[servers.rail.minecraft]
control = "rcon"
readiness = "log"

[servers.rail.minecraft.rcon]
address = "127.0.0.1:25575"
password_file = "~/.config/mcwarden/secrets/rail-rcon-password"

[servers.rail.minecraft.log]
path = "~/minecraft/rail/logs/latest.log"
```

`podman` table、`unit`（`.service`名）、`container`は必須です。`direct`設定は使用しません。責務は次のように分離します。

| 責務 | authority |
| --- | --- |
| Start / escalation coordination | systemd user unit |
| generation observation / wait | Podmanのfull container ID |
| Minecraft graceful stop | 既存RCON |

`start --no-wait`は`systemctl --user start`後、running containerのIDをpinし、readiness observerが設定されていればその世代へlog boundaryをbindして返します。Minecraft readinessの成功を意味しません。`status`はRuntimeのlivenessを確認し、readiness evidenceがあればMinecraftの起動完了を表示します。health/TPSの判定は行いません。`readiness`を設定しない場合、通常の`start` / `restart`はunsupported（終了コード5）です。

初回の`State`はcontainer名からrunning containerを発見したあと、configured Quadlet unitのownershipとcontainerのcgroupを検証してからfull IDをpinします。検証できなければUnknown/errorを返し、pinもRCON stopも行いません。これによりfresh invocationの`status`・`stop`・`restart`も設定不一致をfail-closedで拒否します。

一度pinしたRuntime instanceは、同名containerが再作成されてもreplacementへ自動追従しません。旧IDの終了を待ち、明示的な`Start`成功時だけ次世代をpinします。新しいCLI invocationはownership検証を経て名前から現在の世代を発見できます。Quadletによるauto-removeでWait開始前に旧containerが削除されていた場合、終了確認は成功、exit statusは`Known=false`となります。137等のexit codeからsignalを推測しません。containerが無いのにunitがactive/activating等の場合や、paused/stopping等は安全に停止済みと判断できないためUnknown/errorです。

通常の`stop`はRCON `stop` → 旧IDのWaitです。Minecraftの正常終了（code 0）はunitの`Restart=on-failure`では再起動しない前提です。`restart --no-wait`はその後systemdで起動し、新IDをpinします。

**PodmanのTERM-only escalationはunsupported（exit 5）です。** Quadletのstop jobはMCWardenの待機タイムアウト後も継続し、`StopTimeout`等で暗黙に強制停止する可能性があるため、`--term`だけではstop jobもPodman signalも発行しません。RCON stop後の待機がタイムアウトしてTERM escalationが必要になった場合にunsupportedで終了します。unit設定の書き換えやrestart policyの一時変更も行いません。

`restart --no-wait --kill --force`で強制停止を明示許可した場合だけ、graceful wait timeout後にPodmanのunsupportedなTERM stageをskipし、次の順で処理します。

```text
systemctl --user stop --no-block <verified-unit>
→ podman kill --signal KILL <pinned-ID>
→ Runtime.Wait（同じID）
→ Runtime.Start
```

先にstop jobを入れてsystemdの`Restart=on-failure`によるreplacement起動を抑止します。generic lifecycleでskipできるのは`Signal(Terminate)`の`ErrUnsupported`だけです。他のTERMエラー、Waitのunsupported、RCON失敗や親contextのキャンセルからKILLへ進むことはありません。Directは従来どおりTERM → TERM待機timeout → KILLです。

初回のrunning generation発見時とStart/Killの前に、`systemctl --user show`でloaded/generatedな`.container`由来のunitであること、canonical unit名、生成fragmentが現在のロード内容と一致することを検証します。生成fragmentの`ExecStart`が設定container名を指定する直接の`podman run --name ...`でなければpinもmutationも拒否します。初回pin前・KILL前・Start後には、Podman inspectの`State.CgroupPath`がunitの`ControlGroup`配下であることも確認します。名前や任意のlabelだけをownershipの証拠にしません。

現在のownership検証は安全側に制限しています。systemdのservice drop-in、daemon-reload待ち、global arguments/wrapper付きExecStart、検証できないquote/継続行/環境変数展開等はunsupportedです。Quadletソース側の設定は生成fragmentへ反映してください。実行中containerがunitのdelegated cgroup配下にならない構成（例：`CgroupsMode`の変更）も検証できなければ拒否します。既知のreplacementは停止しませんが、unit/fragment/name/cgroup確認とmutationは原子的ではなく、外部操作との同時変更までは保証できません。

Podmanはhost PIDをidentityとして公開・保存せず、Direct用PID/PGID/starttime/boot_id stateを読みません。container identityの永続化も行いません。`state show` / `state clean`はunsupportedです。一方、lifecycle mutationはDirectと同じserver単位のflockを取得します。readiness evidenceはcontainer identityの永続化ではなく、`readiness/<server>.json`の独立した記録です。

## RCON control

RCONはMinecraftへのcommand transport専用です。Runtimeがプロセスのライフサイクルを所有し、RCONはコマンド送信だけを担当します。RCON adapterにRuntimeの所有権はありません。

RCONはplaintext transportです。loopback、または信頼できる暗号化されたネットワーク内でのみ使用し、インターネットへ直接公開しないでください。

`mcwarden stop`はRCONで`stop`を送信したあと、`Runtime.Wait`でプロセス終了を待ちます。タイムアウトしても`stop`自身はSIGTERM/SIGKILLへescalateしません。通信失敗・認証失敗・呼び出し元のキャンセルもescalationの理由にしません。

`mcwarden restart --no-wait`は次の順に進みます。

```text
RCON "stop"
→ Runtime.Wait
→ 旧世代のidentityをclean（Directのみ）
→ Runtime.Start
→ 新世代のidentityを保存（Directのみ。PodmanはIDをinstance内でpin）
→ 戻る
```

escalationは待機タイムアウト時だけ行い、RCONの通信エラーでは行いません。Directの`restart --term` / `--kill --force`の既存挙動は維持します。PodmanのTERM-onlyは上記のとおりunsupportedです。

RCONの`Command`は「配送を受け付けた」ことを成功とし、コマンドの効果完了までは待ちません。`stop`ではサーバーが応答前に終了し得るため、認証成功後にcommand packetを完全にwriteできた時点で成功とします。終了確認は`Runtime.Wait`が行います。RCON接続はCLI invocationを跨いで維持せず、各`Command`でdial → authenticate → command write → closeします。

RCON接続成功をMinecraft lifecycleの`Running`証拠にはしません。`process running`と`Minecraft running`は別です。readinessは`readiness = "log"`を設定した場合だけ、`latest.log`のstartup完了行を現在のRuntime generationに結び付けて観測します。

### password file

RCON passwordはCLI引数やTOMLへ直接書かず、`password_file`から読みます。ファイルはMCWardenを実行するユーザーだけが読める権限にしてください。

- 末尾の`\n`または`\r\n`だけを除去します。空白などそれ以外の文字はtrimしません。
- 除去後に空になるpasswordはcredential errorです。`stop` / `restart --no-wait`などMinecraft Controlを必要とする操作だけが失敗し、`status`・`state show`・`start --no-wait`は妨げません。
- password fileの読み込みは各RCON `Command`実行時まで遅延します。
- passwordそのものはerror、JSON output、log、テストの失敗メッセージへ出しません。
- `server.properties`の自動解析は行いません。

## Minecraft readiness（generation-scoped）

`readiness = "log"`を設定すると、`start` / `restart`は次の順で新generationの証拠を作ります。

```text
Observer.Prepare（現在のlatest.logのfile identityとoffsetを記録）
→ Runtime.Start
→ Runtime.Generation（opaqueなgeneration key）
→ Observer.Bind（boundaryとgeneration keyをatomicに保存）
→ 通常start/restartは Observer.State でRunningまで待つ
```

`--no-wait`でもPrepareとBindまでは行います。そのため`mcwarden start --no-wait`の後に別invocationの`mcwarden status`を実行すると、同じgenerationの`latest.log`を追跡し、startup完了行の出現後に`Minecraft running`へ昇格できます。

`Generation`はadapter固有のidentityを外側へ解析させないopaque keyです。Podmanはpin済みfull container ID、Directはboot ID・PID・procfs starttimeをsha256で結合します。`Generation`自体はprocess/containerの起動・採用・replacementへのrebindを行いません。

`Prepare`時点でlogが無いことは正常で、boundaryはabsentになります。その後作成されたlogをoffset 0から読みます。同じinodeが継続しsizeが増えていればPrepare時のoffset以降だけを読み、sizeが小さければtruncate、inodeが変わればrotation/recreateとしてoffset 0から読みます。same inodeで旧offsetを超えてregrowしていても、boundary直前の内容fingerprintが一致しなければrewrite/truncateとしてoffset 0から読み直します。Prepareより前の`Done`は証拠にしません。startup完了行はlogger prefixに依存せず、同一行に`Done (`と`For help, type "help"`を含むことを要求します（vanilla / Forge）。

readiness evidenceの更新はserverごとのcross-process flock下で現在recordを再読込して行います。古いgenerationの`status`が新しい`Bind`を上書きすることはなく、`ready=true`をpendingへ巻き戻すこともありません。`restart`は停止前に`Prepare`でlog設定を検証し、設定不良で稼働中serverを止めてから失敗しないようにします。実際のboundaryは`Runtime.Start`直前にもう一度取得します。

readiness evidenceはDirectのprocess identityやPodmanのcontainer identityとは別に、`${XDG_STATE_HOME:-~/.local/state}/mcwarden/readiness/<server>.json`へatomicに保存します（directory 0700、file 0600）。recordはopaqueなgeneration key、ready flag、log boundaryだけを持ち、readiness consumerがserver名やPID/container IDを解析する設計ではありません。

matching generationのevidenceが無い場合やgenerationが一致しない場合は`Minecraft unknown`、matching generationのpending evidenceにmarkerが無ければ`Minecraft starting`、markerを確認して`ready=true`を保存できたら`Minecraft running`です。一度readyになったgenerationはlog rotation/truncateではRunningを取り消しません。generationが変われば古いready evidenceは無効です。

この機能の導入時点ですでに稼働しているserverは、start前boundaryが無いため`Runtime running` / `Minecraft unknown`のままです。次回MCWarden経由で`start`・`restart`・`start --no-wait`・`restart --no-wait`のいずれかにより新generationを開始した時点からevidenceを作ります。既存世代をretroactiveにready認定しません。

readiness timeout、observer error、evidence保存error、context cancellationはsignal escalationの理由にしません。Start成功後のBind/observer errorでserverをSIGTERM/SIGKILLしてrollbackしません。`Running`は「現在のgenerationについてMinecraft startup completionを観測済み」という意味で、healthyを意味しません。

## process identity state（Direct専用）

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

停止はMinecraftの`stop`だけを使用します。再起動時のシグナル送信は`--term`または`--kill --force`で明示した場合に限定し、停止確認後にだけ起動します。既定の`restart`はreadiness observerが設定されていればMinecraftの起動完了まで待ち、無ければ停止前に終了コード5を返します。`--no-wait`ではRCONによるgraceful stop後にプロセスを起動し、readiness observerがあれば新generationへevidenceをbindして返します。

[設計](docs/design.md) / [MIT License](LICENSE)
