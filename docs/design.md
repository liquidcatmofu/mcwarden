# Coreの設計

`internal/app`が操作を組み立て、`runtime.Runtime`がプロセス、`minecraft.Control`がMinecraftコマンドを担当する。実アダプターの前に、この境界をfakeで検証する。

## 停止と再起動

- `stop`は正常停止要求と終了待機だけを行う。タイムアウトしてもシグナルを送らない。
- `restart`の既定動作も正常停止のみ。`--term`でSIGTERMまで、`--kill --force`でSIGKILLまで許可する。
- 指定した段階の待機が期限を超えた場合だけ次へ進む。通信失敗・権限エラー・呼び出し元のキャンセルでは進まない。
- 終了コードが非ゼロでも、プロセス終了を確認できれば再起動できる。
- `restart`は既定でMinecraftの起動完了まで待つ。`--start-timeout`の既定値は120秒。`--no-wait`ではプロセス起動後に返す。
- 起動完了の観測に非対応なら、既定の再起動は停止前にエラーとする。`--no-wait`では観測を要求しない。
- `Result.events`は操作履歴、`Result.state`は最後に確認したライフサイクル状態を表す。`process_started`後は`starting`、起動完了確認後は`running`となる。`running`はhealthの正常判定ではない。
- 起動待ちのタイムアウト・キャンセル・観測エラーではシグナルを送らず、起動完了を報告しない。起動途中でJVM終了を確認したら`stopped`と`startup_exited`、取得できた終了コードを返す。クラッシュ原因は終了コードから断定しない。
- `starting` / `running` / `stopping` / `stopped` / `unknown`をプロセスの生存状態と区別する。起動完了の証拠がない既存プロセスは`unknown`とする。

RuntimeのWaitは期限切れ後も再度呼べる必要がある。シグナルとWaitは同じプロセス世代を対象とする。Start成功後は新しい世代を対象とし、Minecraft Observerもその世代の観測だけを返す。コンテナのラッパーが残っていてもJVM終了を検出する必要がある。実アダプター追加時に、外部の再起動との競合とサーバー単位の操作ロックを実装する。core単体は呼び出し側による直列化を前提とする。

## statusとCLI

Runtimeの状態取得失敗はエラー。Minecraft側の取得失敗は部分結果の`minecraft.error`に残し、statusは成功扱いとする。JSONでは未取得のcapabilitiesをnullにする。

終了コードは0=成功、1=一般エラー、2=引数不正、3=停止中、4=タイムアウト、5=非対応、6=権限不足、9=確認必要、10=外部ツール失敗、130=キャンセル。7/8のhealth判定は未実装。

`-i`・`-y`・確認UI、実Minecraft adapter、health判定、監視・バックアップ、パッケージマネージャー連携は次の段階。`-y`は将来もシグナル送信の許可には使わない。CLIの引数解析エラーはstderr、正常に解析できた`--json`操作の結果はstdoutへJSONで出す。`mcwarden state show`は保存済みidentityと分類を表示し、`mcwarden state clean`はstale stateだけを削除する。`start`/`kill`のCLI配線はstate導入後の段階とする。

## 設定とserver selection

`internal/config`がTOMLを読み、`internal/bootstrap`が選択したserverから`app.Service`を組み立てる。

- 既定パスは`${XDG_CONFIG_HOME:-~/.config}/mcwarden/config.toml`。`-c/--config`で上書きする。
- `-s/--server`、`default_server`、唯一のserverの順に選択する。複数あって指定が無ければエラー。
- `-c`・`-s`・`--json`はコマンドの前後どちらでも置ける。`--`以降はグローバル解析の対象外。
- 未知のTOMLキーはエラーにする。設定のtypoを黙って無視しないため。
- `runtime`は`direct`（既定）と`podman`。`podman`は既知だが未実装として終了コード5を返す。
- `direct.env`は親環境への追加として扱い、同名キーは設定側が上書きする。
- `direct.pidfile`がある場合、内容の`PID`または`PID STARTTIME`から`Adopt`を作る。STARTTIME省略時はその場でprocfsを読み、その時点のstarttimeを固定する。pidfileやプロセスが無ければ「未起動」として採用しない。
- 設定エラーは終了コード2。TOML読み込みに`github.com/BurntSushi/toml`を使う。

## process identity state

MCWardenが起動・採用した世代の同一性は`internal/state`が永続化する。`direct.pidfile`は外部プロセスの発見、このstateはMCWarden管理下の世代identity、という役割分担にし、両者を混ぜない。

- 保存先は`${XDG_RUNTIME_DIR}/mcwarden/<server>.json`。`XDG_RUNTIME_DIR`が無い場合は`/run/user/<uid>`が存在し、現在UID所有・group/other権限なしであることを確認して使う。どちらも安全でなければ設定エラー（終了コード2）とし、`/tmp`には書かない。ディレクトリは0700、stateとlockは0600。
- stateにはPID・PGID・starttime・boot_id・owned・signal_groupを保存する。boot_idが変われば再起動後の別プロセスとみなす。保存は同一ディレクトリの一時ファイル→fsync→renameで行う。
- 分類は、liveかつPID/starttime/PGID/boot_idが一致すれば`managed`、PID消滅・starttime/PGID不一致・boot_id不一致・zombieなら`stale`、procfsやboot_idを読めなければ`unknown`。
- `unknown`は停止済みとみなさず、自動削除も二重起動もしない。`stale`だけを`mcwarden state clean`で明示的に削除できる。`managed`と`unknown`のcleanは拒否する。
- `start`前（`Restart`の起動段階を含む）はserver単位のflockを保持したままstateを検証し、`managed`・`unknown`・`stale`では起動しない。stateが無い場合もruntimeの状態を確認し、adopt済みプロセスの二重起動を防ぐ。
- 起動後は`runtime.Identifiable`からidentityを取得して保存する。保存できなければ、追跡不能なプロセスを残さないよう起動したプロセスを停止する。

## DirectProcessアダプター

`internal/runtime/direct`がLinuxプロセス向けの`runtime.Runtime`を実装する。`/proc`の解析は`internal/host/linux`に隔離する。

- `Start`は子プロセスを新しいプロセスグループで起動する。成功はプロセス起動を意味し、Minecraftの起動完了ではない。
- 世代はPID・PGID・starttimeで識別し、`State`・`Signal`・`Wait`は同じ世代だけを対象にする。
- `Signal`は自分で起動したプロセスにはプロセスグループへ送る。外部から採用したプロセスは既定でPID単体にだけ送り、`Adopt.SignalGroup`を明示した場合のみグループへ送る。
- シグナル直前に`/proc`を読み直し、PID・starttime・PGIDを照合する。不一致ならシグナルを送らずエラーにする。group signalはPGIDが記録と一致し、かつPIDがそのgroup leaderである場合だけ行う。
- 既存プロセスは`Adopt{PID, StartTime}`で採用する。`StartTime`は必須で、一致しない場合はPID再利用とみなして対象にしない。採用中は`Start`を拒否する。
- `Wait`はタイムアウト後も再試行でき、同じ世代を観測する。採用したプロセスは子ではないため終了コードを取得できず、`Known=false`を返す。
- 非0終了やシグナル終了はエラーにしない。終了は`ExitStatus{Known, Code, Signal, CoreDumped}`で表す。`Signal`は終了原因専用の`ExitSignal{Number, Name}`とし、SIGSEGV(11)等の任意シグナルを失わない。操作要求用の`runtime.Signal{term, kill}`とは別型にする。
- adopted processの生存判定は、ENOENT/ESRCH・starttime不一致・zombie/deadだけを終了とし、その他のprocfsエラーは`State`/`Signal`/`Wait`のエラーとして返す。読めないプロセスを停止済みと誤認して二重起動しないため。終了直前のprocfsはENOENTではなくESRCHを返すことがある。
- `Config.Path`はJVM本体、または最後に`exec java ...`でJVMへ置き換わるwrapperを契約とする。process treeは探索せず、wrapperが先に終了する構成やJVMより長生きする構成は対象外。

DirectProcessアダプターはCLIへ配線済みで、`status`は設定したserverを対象に動作する。process identity stateと`state show`/`state clean`も実装済みで、`start`/`kill`の配線時に二重起動防止と保存を有効にする。Minecraft側のcontrol/readiness adapterが未実装のため、`stop`/`restart`は終了コード5を返す。
