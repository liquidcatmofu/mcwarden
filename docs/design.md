# Coreの設計

`internal/app`が操作を組み立て、`runtime.Runtime`がプロセス、`minecraft.Control`がMinecraftコマンドを担当する。実アダプターは`internal/runtime/direct`と`internal/minecraft/rcon`で、境界はfakeでも検証する。RuntimeとMinecraft Controlは責務を分離し、RCON adapterにプロセスの所有権を持たせない。

## 停止と再起動

- `stop`はRCONでMinecraftの`stop`を送り、正常停止要求と終了待機だけを行う。タイムアウトしてもシグナルを送らない。
- `restart`の既定動作も正常停止のみ。`--term`でSIGTERMまで、`--kill --force`でSIGKILLまで許可する。
- 指定した段階の待機が期限を超えた場合だけ次へ進む。通信失敗・認証失敗・権限エラー・呼び出し元のキャンセルでは進まない。
- `Command`の成功は配送受付を意味し、コマンドの効果完了を待たない。`stop`は認証後にcommand packetを完全にwriteできた時点で成功とする。終了確認は`Runtime.Wait`が行う。
- 終了コードが非ゼロでも、プロセス終了を確認できれば再起動できる。
- `restart`は既定でMinecraftの起動完了まで待つ。`--start-timeout`の既定値は120秒。`--no-wait`ではプロセス起動後に返す。
- 起動完了の観測に非対応なら、既定の再起動は停止前にエラーとする。`--no-wait`では観測を要求しない。
- `Result.events`は操作履歴、`Result.state`は最後に確認したライフサイクル状態を表す。`process_started`後は`starting`、起動完了確認後は`running`となる。`running`はhealthの正常判定ではない。
- 起動待ちのタイムアウト・キャンセル・観測エラーではシグナルを送らず、起動完了を報告しない。起動途中でJVM終了を確認したら`stopped`と`startup_exited`、取得できた終了コードを返す。クラッシュ原因は終了コードから断定しない。
- `starting` / `running` / `stopping` / `stopped` / `unknown`をプロセスの生存状態と区別する。起動完了の証拠がない既存プロセスは`unknown`とする。

RuntimeのWaitは期限切れ後も再度呼べる必要がある。シグナルとWaitは同じプロセス世代を対象とする。Start成功後は新しい世代を対象とし、Minecraft Observerもその世代の観測だけを返す。コンテナのラッパーが残っていてもJVM終了を検出する必要がある。`internal/app`はserver単位のflockでmutation操作を直列化し、`runtime.Runtime`単体は呼び出し側による直列化を前提とする。外部の再起動との競合は将来の監視機能で扱う。

## statusとCLI

Runtimeの状態取得失敗はエラー。Minecraft側の取得失敗は部分結果の`minecraft.error`に残し、statusは成功扱いとする。JSONでは未取得のcapabilitiesをnullにする。

終了コードは0=成功、1=一般エラー、2=引数不正、3=停止中、4=タイムアウト、5=非対応、6=権限不足、9=確認必要、10=外部ツール失敗、130=キャンセル。7/8のhealth判定は未実装。

`-i`・`-y`・確認UI、Readiness Observer、health判定、監視・バックアップ、パッケージマネージャー連携は次の段階。`-y`は将来もシグナル送信の許可には使わない。CLIの引数解析エラーはstderr、正常に解析できた`--json`操作の結果はstdoutへJSONで出す。`mcwarden start`は既定でMinecraftの起動完了まで待ち、observerが無ければ起動前に終了コード5を返す。`--no-wait`はプロセス起動後に返る。`mcwarden restart`も既定ではreadiness preflightで終了コード5となり、`--no-wait`だけがRCONによるgraceful stop後に起動まで進む。`mcwarden state show`は保存済みidentityと分類を表示し、`mcwarden state clean`はstale stateだけを削除する。`kill`のCLI配線はまだ。

## 設定とserver selection

`internal/config`がTOMLを読み、`internal/bootstrap`が選択したserverから`app.Service`を組み立てる。

- 既定パスは`${XDG_CONFIG_HOME:-~/.config}/mcwarden/config.toml`。`-c/--config`で上書きする。
- `-s/--server`、`default_server`、唯一のserverの順に選択する。複数あって指定が無ければエラー。
- `-c`・`-s`・`--json`はコマンドの前後どちらでも置ける。`--`以降はグローバル解析の対象外。
- 未知のTOMLキーはエラーにする。設定のtypoを黙って無視しないため。
- `runtime`は`direct`（既定）と`podman`。`podman`は既知だが未実装として終了コード5を返す。
- `[servers.<name>.minecraft]`は任意。無い場合はMinecraft Control unsupported。`control`は現在`rcon`のみ実装する。
- `control = "rcon"`には`[servers.<name>.minecraft.rcon]`と必須の`password_file`が必要。`address`の省略時は`127.0.0.1:25575`。RCON tableやpassword_fileが無い場合、未知のcontrol名は設定エラー。
- `direct.env`は親環境への追加として扱い、同名キーは設定側が上書きする。
- `direct.pidfile`がある場合、内容の`PID`または`PID STARTTIME`から`Adopt`を作る。STARTTIME省略時はその場でprocfsを読み、その時点のstarttimeを固定する。pidfileやプロセスが無ければ「未起動」として採用しない。`managed`な内部stateがある場合はpidfileより優先し、`stale`・`unknown`ではpidfileへfallbackしない。
- 設定エラーは終了コード2。TOML読み込みに`github.com/BurntSushi/toml`を使う。

## Minecraft RCON Control

`internal/minecraft/rcon`が`minecraft.Control`を実装する。RCONはcommand transportであり、プロセスの所有権やreadiness判定を持たない。

- `Capabilities`は`{Command: true, Readiness: false}`。RCON接続成功をMinecraft lifecycleの`Running`証拠にはしない。
- 各`Command`はdial → auth packet送信 → auth response検証 → command packet write → closeを行う。接続はCLI invocationを跨いで維持しない。
- `Command`の成功は配送受付であり、効果完了ではない。`stop`はサーバーが応答前に終了し得るため、認証成功後にcommand packetを完全にwriteできた時点で成功とする。終了確認は`Runtime.Wait`が担当する。
- packetはlittle-endianのlength、request id、type、payload、終端2バイトで構成する。lengthは10未満と上限超過をallocate前に拒否し、truncated packetやmalformed packetは安全にエラーにする。
- context deadlineはsocket deadlineへ反映する。deadlineが無いキャンセルでも、connectionをcloseしてdial/read/writeの待機を中断する。
- authentication failureは専用の`ErrAuthentication`として識別する。通信失敗・認証失敗・キャンセルは`errWaitTimeout`ではないため、`stop`/`restart`のsignal escalationを引き起こさない。
- passwordは`password_file`から読み、CLI引数やTOMLへ置かない。末尾の`\n`または`\r\n`だけを除去し、それ以外はtrimしない。空passwordはcredential error。passwordはerror・JSON・log・テスト失敗メッセージへ出さない。`server.properties`は解析しない。
- `password_file`の読み込みは各`Command`実行時まで遅延する。credentialの不在・読取不能・空passwordはMinecraft Controlのエラーであり、`status`・`state show`・`start --no-wait`を妨げない。credential errorもwait timeoutではないためsignal escalationを引き起こさない。
- RCONはplaintext transportである。loopbackまたは信頼できる暗号化ネットワーク内でのみ使い、インターネットへ直接公開しない。

`minecraft.Observer`はまだ実装しない。RCON endpointが現在のRuntime generationに属するという十分な証拠が無いため、process runningとMinecraft runningを同一視しない。Readinessは次の段階でgeneration-scopedな証拠の設計から行う。

## process identity state

MCWardenが起動・採用した世代の同一性は`internal/state`が永続化する。`direct.pidfile`は外部プロセスの発見、このstateはMCWarden管理下の世代identity、という役割分担にし、両者を混ぜない。

- 保存先は`${XDG_STATE_HOME:-~/.local/state}/mcwarden/<server>.json`。ログインセッションをまたいでidentityを保持する必要があるため、runtime directoryではなく永続state directoryに置く。server単位のlockだけを`${XDG_RUNTIME_DIR}/mcwarden/<server>.lock`に置き、`XDG_RUNTIME_DIR`が無い場合は`/run/user/<uid>`が存在し、現在UID所有・group/other権限なしであることを確認して使う。どちらも安全でなければエラー（終了コード2）とし、`/tmp`には書かない。ディレクトリは0700、stateとlockは0600。state directoryは必要になった時点で作成し、read-onlyの`state show`はruntime directoryが無くても動く。
- stateにはPID・PGID・starttime・boot_id・owned・signal_groupを保存する。boot_idが変われば再起動後の別プロセスとみなす。保存は同一ディレクトリの一時ファイル→fsync→renameで行う。
- 分類は、liveかつPID/starttime/PGID/boot_idが一致すれば`managed`、PID消滅・starttime/PGID不一致・boot_id不一致・zombieなら`stale`、procfsやboot_idを読めなければ`unknown`。
- `unknown`は停止済みとみなさず、自動削除も二重起動もしない。`stale`だけを`mcwarden state clean`で明示的に削除できる。`managed`と`unknown`のcleanは拒否する。
- 次回のinvocationは`managed`の保存済みidentityだけから`direct.Adopt`を組み立て、新しい`DirectRuntime`へ復元する。`status`・`stop`・`restart`・signalが前回起動した世代を対象にできる。復元できるstateは`direct.pidfile`より優先する。
- `unknown`はrecordを保持して`state show`と`status`（`unknown`）だけを許し、`start`・`stop`・`restart`・signalは`ErrStateUnknown`で拒否する。boot_idを検証できない以上、同じbootのプロセスと確定していないためsignalしない。`stale`は`state clean`待ちとし、`absent`だけがpidfile discoveryへ進む。
- `start`前（`Restart`の起動段階を含む）はserver単位のflockを保持したままstateを検証し、`managed`・`unknown`・`stale`では起動しない。stateが無い場合もruntimeの状態を確認し、adopt済みプロセスの二重起動を防ぐ。
- 起動後は`runtime.Identifiable`からidentityを取得して保存する。保存できなければ、追跡不能なプロセスを残さないよう起動したプロセスを停止し、残ったstateも掃除する。
- `start`・`stop`・`restart`・`kill`・`state clean`はserver単位のflockで直列化する。`restart`はstop→wait/escalation→旧state削除→start→新state保存まで同じlockを保持する。内部helperはlockを取らない。
- `Wait`が対象世代の終了を確認した場合、その操作の中で旧identityを削除してから次へ進む。削除できるのは`stale`だけとし、`managed`・`unknown`は別の世代が存在し得るためエラーにする。MCWarden外で終了したプロセスのstateは`state clean`で明示的に削除する。

## DirectProcessアダプター

`internal/runtime/direct`がLinuxプロセス向けの`runtime.Runtime`を実装する。`/proc`の解析は`internal/host/linux`に隔離する。

- `Start`は子プロセスを新しいプロセスグループで起動する。成功はプロセス起動を意味し、Minecraftの起動完了ではない。
- 世代はPID・PGID・starttimeで識別し、`State`・`Signal`・`Wait`は同じ世代だけを対象にする。
- `Signal`は自分で起動したプロセスと、保存済みstateから復元した`Owned`プロセスにはプロセスグループへ送る。外部から採用したプロセスは既定でPID単体にだけ送り、`Adopt.SignalGroup`を明示した場合のみグループへ送る。
- シグナル直前に`/proc`を読み直し、PID・starttime・PGIDを照合する。不一致ならシグナルを送らずエラーにする。group signalはPGIDが記録と一致し、かつPIDがそのgroup leaderである場合だけ行う。
- 既存プロセスは`Adopt{PID, PGID, StartTime, Owned}`で採用する。`StartTime`は必須で、一致しない場合はPID再利用とみなして対象にしない。`PGID`が記録されている場合はgroup不一致をidentity変更としてエラーにする。`Owned`はMCWarden管理下であることを示し、childでwait(2)可能であることとは別概念とする。採用中は`Start`を拒否する。
- `Wait`はタイムアウト後も再試行でき、同じ世代を観測する。今回のinvocationが起動したchildは終了コードを取得できる。採用プロセスと復元した`Owned`プロセスはchildではないため`Known=false`を返す。
- 非0終了やシグナル終了はエラーにしない。終了は`ExitStatus{Known, Code, Signal, CoreDumped}`で表す。`Signal`は終了原因専用の`ExitSignal{Number, Name}`とし、SIGSEGV(11)等の任意シグナルを失わない。操作要求用の`runtime.Signal{term, kill}`とは別型にする。
- adopted processの生存判定は、ENOENT/ESRCH・starttime不一致・zombie/deadだけを終了とし、その他のprocfsエラーは`State`/`Signal`/`Wait`のエラーとして返す。読めないプロセスを停止済みと誤認して二重起動しないため。終了直前のprocfsはENOENTではなくESRCHを返すことがある。
- `Config.Path`はJVM本体、または最後に`exec java ...`でJVMへ置き換わるwrapperを契約とする。process treeは探索せず、wrapperが先に終了する構成やJVMより長生きする構成は対象外。

DirectProcessアダプターはCLIへ配線済みで、`status`・`start --no-wait`・`state show`/`state clean`が動作する。保存済みstateからの復元、終了確認後のstate削除、server単位のmutation lockも実装済み。Minecraft RCON Controlも配線済みで、`stop`と`restart --no-wait`がRCON `stop` → `Runtime.Wait` → 新process起動まで進む。Readiness Observerが未実装のため、既定の`start`・`restart`は起動前（restartは停止前）に終了コード5を返す。`kill`のCLI配線はまだ。
