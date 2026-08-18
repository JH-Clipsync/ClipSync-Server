<p align="center">
  <img src="icon.png" width="128" alt="ClipSync アイコン"/>
</p>

<h1 align="center">ClipSync-Server</h1>

<p align="center">
  <b>ClipSync 3端末同期システムのリレーサーバー</b><br/>
  <a href="README.md">简体中文</a> ·
  <a href="README.en.md">English</a> ·
  <a href="README.ja.md">日本語</a>
</p>

---

ClipSync-Server は、ClipSync のセルフホスト型クロスデバイスメッセージ同期システムの中核となるリレーサービスです。**Go + gorilla/websocket + MySQL + Redis** で開発されています。スマートフォンが受信した**SMS認証コード**やコピーした**テキスト/画像**を PC へリアルタイムに転送し、その逆も行います。

サードパーティのプッシュサービスには一切依存せず、すべてのトラフィックは専用の WebSocket チャネルを通ります。エンドツーエンド暗号化は任意で、プライバシーはユーザー自身が管理できます。デフォルトでは **28001** ポートで待ち受けます。

---

## ✨ 主な機能

| モジュール | 説明 |
|------------|------|
| 🔄 **WebSocket リアルタイム転送** | `userID` グループごとにルーティングし、`notify_pc` / `notify_mobile` / `notify_all` / `clipboard` の4つの配信セマンティクスをサポート。`pc` と `mobile` ロールが相互に送信し、ループすることはありません |
| 👥 **オンラインデバイス管理** | メモリ上の Hub が実際の WebSocket 接続を権威的に維持。Redis Hash（`clipsync:online:<userID>`、TTL 90秒）がオンライン状態を記録し、30秒ごとのハートビートで更新します。接続切断時には自動的にクリーンアップされ、プロセスが kill されてもゴーストレコードは残りません |
| 📡 **Presence リアルタイム配信** | デバイスのオンライン/オフライン時に、同じグループのすべての接続へ `presence` メッセージをプッシュし、クライアントがオンラインデバイス UI（プラットフォーム / IP / 機能ビット / カスタム名）をリアルタイムに更新します |
| 👤 **ユーザーシステム** | 登録 / ログイン、scrypt パスワードハッシュ（N=32768, r=8, p=1）、ランダムトークン（32バイト、SHA-256 ハッシュのみ保存）、トークン TTL（デフォルト720時間 / 30日）、IP 単位のログインレート制限 |
| 📱 **デバイステーブル管理** | `devices` テーブルにアカウント配下のデバイス（ロール / プラットフォーム / カスタム名 / 最終 IP）を永続化。初回ハンドシェイク時に自動で登録されます。管理者がデバイスを**無効化**すると、その後のハンドシェイクは拒否されます |
| 👟 **強制ログアウト** | ユーザー単位で全デバイスを蹴る、デバイス単位で1台を蹴る操作に対応。パスワードリセット / ユーザーBAN / ユーザー削除時にも連動してログアウト。5種類の kick reason（パスワードリセット / ユーザーBAN / ユーザー削除 / デバイスキック / デバイス無効化） |
| 🛡️ **管理API** | `GET /server-admin/users/{id}/devices`（オンライン状態はメモリ Hub を正、Redis で補足）、デバイスの有効/無効・リネーム、全デバイスのページネーション検索、統一アクションエンドポイント `POST /server-admin/kick`。Bearer Token を定数時間比較で認証 |
| 📨 **Redis Pub/Sub 連携** | ClipSync-Admin と Redis を共有している場合、チャネル `clipsync:admin:kick_user` 経由で制御コマンドを配信し、HTTP API をフォールバックとする二重構成 |
| 🧹 **SMS ペイロードクリーニング** | `【+86xxx】` / `[N条]` プレフィックスを自動で剥離し、11桁の送信元携帯番号を `sender` に抽出、空白を trim します。下流クライアントでモバイク側の注入を処理する必要はありません |
| 🔐 **エンドツーエンド暗号化ゲート** | `e2ee.require=true` の場合、平文メッセージの転送を拒否し、`/push` 平文エンドポイントを閉じます。暗号文はサーバーが中継するだけで、内容は見えません |
| 📝 **日次ログローテーション** | 汎用ログ `logs/clipsync.log` とメッセージ配信ログ `logs/message.log` を、日次で `logs/clipsync/`、`logs/message/` サブディレクトリにアーカイブ。保持日数は設定可能 |
| 🐳 **Docker ネイティブ** | マルチステージビルド → distroless nonroot イメージ（シェルなし、非 root、攻撃面最小）。host ネットワークでホストの MySQL/Redis に直接接続でき、volume で設定とログを永続化 |

---

## 🚀 クイックスタート

### 方法1：Docker Compose（推奨）

リポジトリ同梱の `docker-compose.yml` は **host ネットワーク**を使用し、ホスト上の既存 MySQL / Redis に直接接続します。追加のコンテナは起動しません：

```bash
# 1. 設定の準備
mkdir -p config logs
cp deploy/config.external.yaml config/config.yaml
# config/config.yaml を編集し、mysql.password / redis.password / admin_token を記入

# 2. .env の準備（初期アカウントのパスワードなどを決定）
cp .env.example .env
vim .env   # 最低限 BOOTSTRAP_PASSWORD を変更

# 3. 起動
docker compose up -d
docker compose logs -f clipsync
```

起動に成功すると：

- サービスは `:28001` で待ち受けます（host ネットワークがホストポートに直接バインド）
- 初期アカウントは `.env` の `BOOTSTRAP_USER` / `BOOTSTRAP_PASSWORD` で指定
- 設定ファイルは `./config/config.yaml` にマウントされ、ログは `./logs/` に書き込まれます

> Compose で MySQL + Redis + Server を一括起動したい場合は、`deploy/config.compose.yaml` を参考にサービス定義を拡張してください。

### 方法2：Docker ワンライナー

```bash
docker run -d --name clipsync-server \
  --network host \
  --restart unless-stopped \
  -v $(pwd)/config:/data/config:ro \
  -v $(pwd)/logs:/data/logs \
  -e TZ=Asia/Shanghai \
  -e CLIPSYNC_TRUST_PROXY=true \
  ghcr.io/jh-clipsync/clipsync-server:latest
```

### 方法3：バイナリ + systemd

[Releases](https://github.com/JH-Clipsync/ClipSync-Server/releases) からプラットフォームに合った tar ボール（`linux-amd64` / `linux-arm64` / `darwin-arm64`）をダウンロードし、展開して以下を実行します：

```bash
sudo ./install.sh
```

スクリプトは以下を行います：

1. `/opt/clipsync-server/` にインストール
2. `clipsync-server.service`（systemd）を登録して起動
3. ブート時自動起動と失敗時自動再起動を設定
4. ログを `/opt/clipsync-server/logs/clipsync.log` に出力

よく使うコマンド：

```bash
sudo systemctl status clipsync-server
sudo systemctl restart clipsync-server
tail -f /opt/clipsync-server/logs/clipsync.log
```

### 方法4：ソースからビルド

Go 1.23 以上が必要です：

```bash
git clone https://github.com/JH-Clipsync/ClipSync-Server.git
cd ClipSync-Server

# 直接実行
go run .

# または静的バイナリをビルド
CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=1.0.0" -o clipsync-server .
./clipsync-server --config config.yaml
```

バージョン確認 / 完全なデフォルト設定のエクスポート：

```bash
./clipsync-server --version
./clipsync-server --print-default-config > config.yaml
```

---

## ⚙️ 設定

設定は `go:embed` によって [config.default.yaml](config.default.yaml) がバイナリに埋め込まれ、唯一のデフォルト値ソースとなります。設定の優先順位：

```
内蔵デフォルト値  <  config.yaml  <  環境変数
```

設定ファイルの検索順序：

1. `--config` コマンドライン引数
2. `CLIPSYNC_CONFIG` 環境変数
3. `./config.yaml`
4. `/etc/clipsync/config.yaml`

ファイルが存在しなくてもエラーにはならず、デフォルト値が使用されます。全フィールドは `clipsync-server --print-default-config` で確認できます。

### 主な設定セクション

| セクション | キー | 説明 |
|------------|------|------|
| `server` | `addr: ":28001"` | 待ち受けアドレス |
| | `trust_proxy: false` | リバースプロキシ配下で `true` にすると、`X-Forwarded-For` から実 IP を取得します |
| | `admin_token: ""` | `/server-admin/*` エンドポイントの Bearer Token。空の場合、管理APIはすべて 503 を返します。`openssl rand -hex 32` での生成を推奨 |
| `logs` | `dir` / `level` / `stdout` / `max_age_days` | ログディレクトリ、レベル（debug/info/warn/error）、stdout への同時出力の有無、アーカイブ保持日数（0=無期限） |
| `websocket` | `read_limit: 10MB` | 1メッセージあたりの上限。クリップボード画像は大きくなる可能性があります |
| | `ping_interval_sec: 30` | サーバー Ping 間隔 |
| | `read_deadline_sec: 60` | 読み取りタイムアウト。死んだ接続がクリーンアップされるまでの時間を決めます |
| | `send_queue_size: 32` | クライアントごとの送信キュー長 |
| `mysql` | `host/port/user/password/database` | ユーザー / セッション / デバイスの永続化。起動時に自動でテーブル作成 |
| | `max_open_conns / max_idle_conns` | コネクションプールのチューニング |
| `redis` | `addr / password / db` | トークンキャッシュ + オンラインデバイス登録 + Pub/Sub |
| | `key_prefix: "clipsync:"` | すべてのキーの統一プレフィックス |
| | `online_ttl_sec: 90` | オンライン登録 TTL。接続中は TTL/3（約30秒）ごとにハートビートで更新 |
| `auth` | `token_ttl_hours: 720` | トークン有効期間（30日） |
| | `allow_register: false` | `POST /auth/register` を公開するかどうか。デフォルトは無効で、アカウントは管理者が作成 |
| | `min_password_len: 8` | 最小パスワード長 |
| | `bootstrap_user / bootstrap_password` | 起動時に自動作成される初期アカウント（既に存在する場合はスキップ） |
| | `login_rate_limit_per_min: 10` | ブルートフォース対策の IP 単位の1分あたりログイン試行上限（0=無制限） |
| `e2ee` | `require: false` | `true` の場合、平文メッセージの転送を拒否し、`/push` 平文エンドポイントを無効化 |
| `message_protocol` | `check_origin: true` | 任意の Origin からの WebSocket ハンドシェイクを許可するかどうか（本番ではホワイトリスト方式への変更を推奨） |
| | `max_payload_preview: 40` | ログに出力するペイロードプレビューの切り詰め文字数 |

### 環境変数によるオーバーライド

いくつかの主要な実行パラメータは環境変数で上書きできます：

| 環境変数 | 対応フィールド |
|----------|----------------|
| `CLIPSYNC_ADDR` | `server.addr` |
| `CLIPSYNC_LOG_DIR` | `logs.dir` |
| `CLIPSYNC_LOG_LEVEL` | `logs.level` |
| `CLIPSYNC_TRUST_PROXY` | `server.trust_proxy` |
| `CLIPSYNC_WS_READ_LIMIT` | `websocket.read_limit` |
| `CLIPSYNC_TOKEN_TTL_HOURS` | `auth.token_ttl_hours` |
| `CLIPSYNC_ALLOW_REGISTER` | `auth.allow_register` |
| `CLIPSYNC_BOOTSTRAP_USER` | `auth.bootstrap_user` |
| `CLIPSYNC_BOOTSTRAP_PASSWORD` | `auth.bootstrap_password` |
| `CLIPSYNC_E2EE_REQUIRE` | `e2ee.require` |

> MySQL / Redis の接続情報は意図的に環境変数での上書きを提供していません。設定ファイルを編集して再起動してください。「設定を変えたのに残っていた環境変数に知らないうちに上書きされた」を防ぐためです。

---

## 📡 API 仕様

### クライアントAPI（ユーザートークンで認証）

| メソッド | パス | 説明 |
|----------|------|------|
| GET | `/ws` | WebSocket アップグレードエンドポイント。クエリパラメータ `token` / `device` / `role`（pc/mobile、旧値 phone も互換） / `platform` / `caps` / `name` |
| POST | `/auth/register` | 登録（`auth.allow_register` スイッチで制御） |
| POST | `/auth/login` | ログイン、トークンを返却。既存のクライアントがオンラインの場合は同じトークンを再利用し、`reused=true` を返却 |
| POST | `/auth/logout` | 現在のトークンを無効化 |
| GET | `/auth/session` | 現在のセッション状態とオンラインデバイスを照会 |
| POST | `/auth/change-password` | パスワード変更。旧トークンは即時無効化され全デバイスがログアウト、新しいトークンを返却 |
| POST | `/push` | 簡易 HTTP プッシュエンドポイント（curl デバッグ用、`e2ee.require=true` 時は無効） |
| POST | `/device/name` | ユーザーが自身のデバイスをリネーム（presence をリアルタイムブロードキャスト） |
| GET | `/health` | ヘルスチェック、`ok` を返却 |

### 管理API（`admin_token` で認証）

すべての `/server-admin/*` エンドポイントには以下のヘッダーが必要です：

```
Authorization: Bearer <server.admin_token>
```

| メソッド | パス | 説明 |
|----------|------|------|
| POST | `/server-admin/users` | 管理者によるユーザー作成（`allow_register` の制限を受けない） |
| GET | `/server-admin/users/{id}/devices` | ユーザーの全デバイスを一覧。**オンライン状態はメモリ Hub を正とし、Redis で補足** |
| GET | `/server-admin/devices` | ユーザーをまたいだページネーション検索。`keyword` / `disabled` / `user_id` / `page` / `page_size` をサポート |
| PUT | `/server-admin/users/{id}/devices/{deviceID}/status` | デバイスの有効化/無効化。無効化すると即座にログアウト |
| PUT | `/server-admin/users/{id}/devices/{deviceID}/name` | デバイスをリネームし、オンライン接続へ presence をブロードキャスト |
| POST | `/server-admin/kick` | 統一アクションエンドポイント。body は `kick_user` / `kick_device` / `disable_device` / `enable_device` をサポート |

### WebSocket メッセージタイプ

| `type` | 配信範囲 | 代表的なユースケース |
|--------|----------|----------------------|
| `notify_pc` | 同グループの全 `pc` ロール | SMS認証コードを PC に同期 |
| `notify_mobile` | 同グループの全 `mobile` ロール | PC からスマホへ通知をプッシュ |
| `notify_all` | 同グループの全デバイス（自分を除く） | 汎用ブロードキャスト |
| `clipboard` | 同グループの全デバイス（自分を除く） | クリップボードテキスト / 画像。受信側が設定に応じて自動書き込みするかを決定 |
| `presence` | サーバーからクライアントへのみ | オンラインデバイスリスト変更通知 |
| `server_kick` | サーバーからクライアントへのみ | 強制ログアウト。`reason` フィールドを伴う |

### Redis Pub/Sub チャネル

- チャネル名：`{redis.key_prefix}admin:kick_user`（デフォルト `clipsync:admin:kick_user`）
- メッセージボディは JSON：`{"action":"kick_user|kick_device|disable_device|enable_device","user_id":1,"device_id":"...","reason":"..."}`
- 数値のみの `userID` とも互換（`kick_user` と等価）
- Admin がパブリッシャー、Server がサブスクライバー。自動再接続あり（指数バックオフ、最大30秒）

---

## 🏗️ プロジェクト構成

```
┌──────────────┐  WebSocket   ┌──────────────────────────────┐
│  PC/Mobile   │ ──────────▶  │      ClipSync-Server         │
│   Clients    │ ◀──────────  │  ┌────────────────────────┐  │
└──────────────┘   presence   │  │  Hub（メモリ）         │  │
                              │  │  userID -> []*Client   │  │
                              │  └──────────┬─────────────┘  │
                              │             │                │
                              │  ┌──────────▼─────────────┐  │
                              │  │  AuthService           │  │
                              │  │  ┌──────┐  ┌────────┐  │  │
                              │  │  │MySQL │  │ Redis  │  │  │
                              │  │  └──────┘  └────────┘  │  │
                              │  └────────────────────────┘  │
                              └──────────┬───────────────────┘
                                         │ Redis Pub/Sub + HTTP
                                         ▼
                              ┌──────────────────────────────┐
                              │       ClipSync-Admin         │
                              └──────────────────────────────┘
```

### データモデル

- **users**：アカウント。パスワードは scrypt ハッシュのみ保存、`disabled` で BAN を制御
- **sessions**：1ユーザーにつき最大1件のアクティブセッション（`user_id` が主キー）。全デバイスが同じトークンを共有。トークンは SHA-256 ハッシュのみ保存
- **devices**：`(user_id, device_id)` 複合主キー。ロール / プラットフォーム / カスタム名 / 最終 IP / 無効状態 / 最終オンライン時刻を記録

### コード構成

| ファイル | 役割 |
|----------|------|
| [main.go](main.go) | Hub / Client / WebSocket ルーティング / presence ブロードキャスト / ハートビート / HTTP ルートエントリ / グレースフルシャットダウン |
| [config.go](config.go) / [config.default.yaml](config.default.yaml) | 設定構造体、読み込み順序、環境変数オーバーライド |
| [auth_service.go](auth_service.go) / [auth_http.go](auth_http.go) / [auth_crypto.go](auth_crypto.go) | ログイン / 登録 / セッション / scrypt + トークンハッシュ / レート制限 |
| [store_mysql.go](store_mysql.go) | ユーザー / セッション永続化と自動テーブル作成 |
| [store_device.go](store_device.go) | デバイステーブル CRUD、ページネーション検索、無効状態 |
| [store_redis.go](store_redis.go) | トークンキャッシュ、オンライン登録、Pub/Sub 管理チャネル |
| [admin_http.go](admin_http.go) | `/server-admin/*` エンドポイント実装 |
| [e2ee.go](e2ee.go) | エンドツーエンド暗号化ポリシーゲート |
| [logger.go](logger.go) | 日次ローテーション + アーカイブ保持 + 汎用/メッセージの2重ログ |
| [device_name_http.go](device_name_http.go) | ユーザー自身によるデバイスリネーム |

---

## 🔐 セキュリティについて

- **パスワード保存**：scrypt（N=32768, r=8, p=1、32バイト派生キー、16バイトランダムソルト）、形式 `scrypt$N$r$p$salt$dk`。パラメータはハッシュと共に保存され、将来的にスムーズにアップグレード可能
- **トークン保存**：32バイトのランダムトークン。MySQL / Redis には SHA-256 ハッシュのみ保存。平文トークンは「同一アカウントの複数デバイスで同じセッションを再利用」するために TTL 付きで Redis に一時的に置かれるだけです
- **ログインレート制限**：IP 単位のスライディングウィンドウ、デフォルト10回/分。ユーザー名エラーとパスワードエラーは同一エラーを返し、ユーザー名列挙を防止
- **管理API認証**：`admin_token` が空なら 503。比較は `subtle.ConstantTimeCompare` で行い、タイミング攻撃を防止
- **リバースプロキシ信頼**：`trust_proxy=false` の場合、`X-Forwarded-For` を無視し、偽造ヘッダーによる欺瞞を防止
- **WebSocket Origin**：本番環境では `message_protocol.check_origin` を `false` にし、ホワイトリストで検証することを推奨
- **エンドツーエンド暗号化**：3端末すべてが暗号化対応バージョンにアップグレードした後、`e2ee.require` を `true` にすると、サーバーは平文メッセージを一切拒否します
- **コンテナセキュリティ**：distroless nonroot イメージ、シェルなし、パッケージマネージャなし、uid 65532 の非 root で実行
- **通信セキュリティ**：本番環境では必ず手前に Nginx / Caddy を置いて TLS を終端してください（以下の例を参照）

### Nginx リバースプロキシの例

```nginx
location = /clipsync/ws {
    proxy_pass http://127.0.0.1:28001/ws;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;
}

location /clipsync/ {
    proxy_pass http://127.0.0.1:28001/;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

`config.yaml` で `server.trust_proxy` を `true` に設定するのを忘れないでください。

### Caddy リバースプロキシの例

リポジトリに [Caddyfile.example](Caddyfile.example) が用意されています：

```
clipsync.example.com {
    reverse_proxy 127.0.0.1:28001
}
```

Caddy は自動的に Let's Encrypt 証明書を申請・更新します。

---

## 🐛 トラブルシューティング

| 現象 | 確認項目 |
|------|----------|
| クライアントのハンドシェイクが 401 を返す | トークンが無効か期限切れ。Redis に `clipsync:token:*` が存在するか、サーバー時刻が正確かを確認 |
| クライアントのハンドシェイクが 403「デバイスは管理者により無効化されています」を返す | `devices.disabled=1`。管理画面でデバイスを有効化 |
| クライアントが接続後すぐ切断される | `websocket.read_deadline_sec` が小さすぎないか、リバースプロキシが Ping/Pong フレームを飲み込んでいないか、Nginx に `proxy_read_timeout` が設定されているかを確認 |
| オンライン状態が正確でない | メモリ Hub が権威ソースです。Redis オンライン TTL はデフォルト90秒で、30秒ごとに更新されます。Redis がクリアされた場合は再接続後に自動回復します。プロセスが異常終了した場合は90秒待てばキーが自然に期限切れになります |
| 管理APIが 503 を返す | `server.admin_token` が未設定。設定後に再起動 |
| 管理APIが 401 を返す | `Authorization: Bearer <token>` ヘッダーがサーバーの `admin_token` と一致しない |
| ログインで「試行回数が多すぎます」と表示 | IP 単位のレート制限が発動。1分待つか、`auth.login_rate_limit_per_min` を大きくする |
| リバースプロキシ配下でログの IP がすべて 127.0.0.1 になる | `server.trust_proxy` を `true` にし、リバースプロキシが `X-Forwarded-For` を設定していることを確認 |
| クリップボード画像が受信できない | `websocket.read_limit`（デフォルト10MB）とリバースプロキシの `client_max_body_size` を確認 |
| Docker コンテナが MySQL / Redis に接続できない | host ネットワークでは `127.0.0.1:<ホストポート>` を指定。MySQL が `127.0.0.1` からの接続を許可しているか、ファイアウォールでポートがブロックされていないかを確認 |
| ログファイルが生成されない | `logs.dir` のディレクトリ権限を確認。distroless nonroot イメージでは uid 65532 がログディレクトリに書き込める必要があります。compose では `user: "0:0"` でフォールバックしています |

### ログの場所

- 汎用ログ：`logs/clipsync.log`（当日）+ `logs/clipsync/clipsync-YYYY-MM-DD.log`（アーカイブ）
- メッセージ配信ログ：`logs/message.log`（当日）+ `logs/message/message-YYYY-MM-DD.log`（アーカイブ）

メッセージログは独立したファイルで、各メッセージの受信 / 送信 / 破棄、業務分類（SMS / クリップボード / 通知）、コンテンツ形式（テキスト / 画像）、配信範囲を記録し、監査とトラブルシューティングを容易にします。

---

## 🤝 関連プロジェクト

- [ClipSync-Admin](https://github.com/JH-Clipsync/ClipSync-Admin)：管理バックエンド（Go + Gin + GORM）
- [ClipSync-Admin-Web](https://github.com/JH-Clipsync/ClipSync-Admin-Web)：管理フロントエンド（Vue 3 + Element Plus）
- [ClipSync-Windows](https://github.com/JH-Clipsync/ClipSync-Windows)：Windows クライアント
- [ClipSync-Mac](https://github.com/JH-Clipsync/ClipSync-Mac)：macOS クライアント
- [ClipSync-Android](https://github.com/JH-Clipsync/ClipSync-Android)：Android クライアント
