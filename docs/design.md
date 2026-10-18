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

`-i`・`-y`・確認UI、TOML設定、実アダプター、health判定、監視・バックアップ、パッケージマネージャー連携は次の段階。`-y`は将来もシグナル送信の許可には使わない。CLIの引数解析エラーはstderr、正常に解析できた`--json`操作の結果はstdoutへJSONで出す。

## DirectProcessアダプター

`internal/runtime/direct`がLinuxプロセス向けの`runtime.Runtime`を実装する。`/proc`の解析は`internal/host/linux`に隔離する。

- `Start`は子プロセスを新しいプロセスグループで起動する。成功はプロセス起動を意味し、Minecraftの起動完了ではない。
- 世代はPID・PGID・starttimeで識別し、`State`・`Signal`・`Wait`は同じ世代だけを対象にする。
- `Signal`は自分で起動したプロセスにはプロセスグループへ送る。外部から採用したプロセスは既定でPID単体にだけ送り、`Adopt.SignalGroup`を明示した場合のみグループへ送る。
- 既存プロセスは`Adopt{PID, StartTime}`で採用する。starttimeが一致しない場合はPID再利用とみなし、そのPIDを対象にしない。採用中は`Start`を拒否する。
- `Wait`はタイムアウト後も再試行でき、同じ世代を観測する。採用したプロセスは子ではないため終了コードを取得できず、`Known=false`を返す。
- 非0終了やシグナル終了はエラーにしない。`ExitStatus{Known, Signaled, Signal}`で表現する。シグナル終了時は`Code=-1`、`Signal`に`term`/`kill`が入る。

DirectProcessアダプターはまだCLIへ配線していない。TOML設定とserver selectionの後に配線する。
