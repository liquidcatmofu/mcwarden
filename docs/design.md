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

`-i`・`-y`・確認UI、実Minecraft adapter、health判定、監視・バックアップ、パッケージマネージャー連携は次の段階。`-y`は将来もシグナル送信の許可には使わない。CLIの引数解析エラーはstderr、正常に解析できた`--json`操作の結果はstdoutへJSONで出す。

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

## DirectProcessアダプター

`internal/runtime/direct`がLinuxプロセス向けの`runtime.Runtime`を実装する。`/proc`の解析は`internal/host/linux`に隔離する。

- `Start`は子プロセスを新しいプロセスグループで起動する。成功はプロセス起動を意味し、Minecraftの起動完了ではない。
- 世代はPID・PGID・starttimeで識別し、`State`・`Signal`・`Wait`は同じ世代だけを対象にする。
- `Signal`は自分で起動したプロセスにはプロセスグループへ送る。外部から採用したプロセスは既定でPID単体にだけ送り、`Adopt.SignalGroup`を明示した場合のみグループへ送る。
- 既存プロセスは`Adopt{PID, StartTime}`で採用する。`StartTime`は必須で、一致しない場合はPID再利用とみなして対象にしない。採用中は`Start`を拒否する。
- `Wait`はタイムアウト後も再試行でき、同じ世代を観測する。採用したプロセスは子ではないため終了コードを取得できず、`Known=false`を返す。
- 非0終了やシグナル終了はエラーにしない。終了は`ExitStatus{Known, Code, Signal, CoreDumped}`で表す。`Signal`は終了原因専用の`ExitSignal{Number, Name}`とし、SIGSEGV(11)等の任意シグナルを失わない。操作要求用の`runtime.Signal{term, kill}`とは別型にする。
- adopted processの生存判定は、ENOENT・starttime不一致・zombie/deadだけを終了とし、その他のprocfsエラーは`State`/`Signal`/`Wait`のエラーとして返す。読めないプロセスを停止済みと誤認して二重起動しないため。
- `Config.Path`はJVM本体、または最後に`exec java ...`でJVMへ置き換わるwrapperを契約とする。process treeは探索せず、wrapperが先に終了する構成やJVMより長生きする構成は対象外。

DirectProcessアダプターはCLIへ配線済みで、`status`は設定したserverを対象に動作する。Minecraft側のcontrol/readiness adapterが未実装のため、`stop`/`restart`は終了コード5を返す。
