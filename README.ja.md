<p align="center">
  <img src="icon.png" width="128" alt="ClipSync ロゴ"/>
</p>

<h1 align="center">ClipSync-Server</h1>

<p align="center">
  <b>ClipSync クロスデバイス同期システムの中継サーバー</b><br/>
  <a href="README.md">简体中文</a> ·
  <a href="README.en.md">English</a> ·
  <a href="README.ja.md">日本語</a>
</p>

---

ClipSync-Server は、セルフホスト型 ClipSync メッセージ同期システムのコア中継サーバーで、**Go + gorilla/websocket + MySQL + Redis** で構築されています。スマートフォンが受信した**SMS認証コード**やコピーした**テキスト/画像**をリアルタイムで PC クライアントへ転送し、その逆も行います。

サードパーティのプッシュサービスには依存せず、すべての通信はあなた自身の WebSocket エンドポイントを経由します。エンドツーエンド暗号化も任意で有効化でき、データはあなたの管理下に置かれます。

---

## ✨ 主な機能

| 分類 | 内容 |
|------|------|
| 🔄 **リアルタイム WebSocket 中継** | `userID` 単位でルーティング。`notify_pc` / `notify_mobile` / `notify_all` / `clipboard` の4種類の配送セマンティクス。`pc` と `mobile` ロール間で相互に配送 |
| 👥 **オンライン端末管理** | メモリ上の Hub が実際の WebSocket 接続を権威的に管理。Redis Hash（`clipsync:online:<userID>`、TTL 90秒）で在席状況を記録し、プロセスが kill されても自然に失効 |
| 📡 **プレゼンスプッシュ** | 端末の接続/切断時に同グループの全接続へ `presence` メッセージをブロードキャストし、クライアントのオンライン端末 UI（プラットフォーム/IP/機能/カスタム名）をリアルタイム更新 |
| 👤 **ユーザーシステム** | 登録/ログイン、scrypt パスワードハッシュ（N=32768, r=8, p=1）、JWT 形式トークン、設定可能な TTL（デフォルト30日）、IP 単位のログインレート制限 |
| 📱 **端末テーブル管理** | `devices` テーブルに各アカウントの端末（ロール/プラットフォーム/カスタム名/最終 IP）を永続化。管理者による**端末の無効化**に対応し、無効化後はハンドシェイクを拒否 |
| 👟 **強制切断** | ユーザー全端末または単一端末をキック可能。パスワードリセット/ユーザー凍結/ユーザー削除時に連動。5種類の kick reason（password_reset / user_disabled / user_deleted / device_kicked / device_banned） |
| 🛡️ **管理 API** | `GET /server-admin/users/{id}/devices`（オンライン状態はメモリ Hub 準拠）、端末の有効/無効/リネーム、全ユーザー横断のページング検索、統一エントリ `POST /server-admin/kick`。Bearer トークン認証（定数時間比較） |
| 📨 **Redis Pub/Sub 連携** | ClipSync-Admin と Redis を共有する場合、チャネル `clipsync:admin:kick_user` 経由で制御コマンドを配信。HTTP API をフォールバックとして二重化 |
| 🧹 **SMS ペイロード整形** | `【+86xxx】` / `[N件]` プレフィックスを除去、11桁の送信者番号を `sender` に抽出、空白をトリム。下流クライアントはキャリア由来のノイズ処理が不要 |
| 🔐 **E2EE ゲート** | `e2ee.require=true` の場合、平文メッセージの中継を拒否し `/push` エンドポイントも無効化。暗号文はそのまま転送 |
| 📝 **日次ローテートログ** | 汎用ログ `logs/clipsync.log` とメッセージ中継監査 `logs/message.log` を日次で `logs/clipsync/`・`logs/message/` にアーカイブ。保持期間を設定可能 |
| 🐳 **Docker ネイティブ** | マルチステージビルドで distroless nonroot イメージ（約20MB）を生成。ホストネットワークでホスト上の MySQL/Redis に直接接続、volume で設定とログを永続化 |

---

## 🏗️ 技術スタック

- **言語**: Go 1.23
- **WebSocket**: [gorilla/websocket](https://github.com/gorilla/websocket) v1.5
- **データベース**: MySQL 8（ユーザー/セッション/端末を永続化、起動時に自動マイグレーション）
- **キャッシュ**: Redis 7（トークンキャッシュ + オンライン端末登録 + Pub/Sub 制御チャネル）
- **パスワードハッシュ**: scrypt（`golang.org/x/crypto/scrypt`）
- **設定**: YAML + `go:embed` によるデフォルト同梱、環境変数オーバーライド対応
- **実行イメージ**: `gcr.io/distroless/base-debian12:nonroot`（シェルなし、非 root）

---

## 🚀 クイックスタート

### 方法1: Docker Compose（推奨）

ルートの `docker-compose.yml` はホストネットワークを使用し、ホスト上で稼働中の MySQL/Redis に接続します。

```bash
git clone https://github.com/JH-Clipsync/ClipSync-Server.git
cd ClipSync-Server

cp .env.example .env
# .env を編集 — BOOTSTRAP_PASSWORD は必須、TOKEN_TTL_HOURS / ALLOW_REGISTER も必要に応じて調整

mkdir -p config logs
cp deploy/config.external.yaml config/config.yaml
# config/config.yaml の mysql.password / redis.password を実際の値に編集

docker compose up -d
docker compose logs -f clipsync
```

起動確認:

```bash
curl http://127.0.0.1:28001/health   # "ok" が返ればOK
```

デフォルトの待受アドレスは `:28001`。初期アカウントは `.env` の `BOOTSTRAP_USER` / `BOOTSTRAP_PASSWORD` で指定します。

### 方法2: 公式イメージを取得

```bash
docker run -d --name clipsync-server \
  --network host \
  -v $(pwd)/config:/data/config:ro \
  -v $(pwd)/logs:/data/logs \
  -e TZ=Asia/Shanghai \
  ghcr.io/jh-clipsync/clipsync-server:latest
```

イメージレジストリ: [ghcr.io/jh-clipsync/clipsync-server](https://github.com/orgs/JH-Clipsync/packages)

### 方法3: ソースからビルド

```bash
# 前提: Go 1.23以上、到達可能な MySQL 8 と Redis
go build -ldflags "-X main.version=1.0.0" -o clipsync-server .
./clipsync-server --print-default-config > config.yaml
# config.yaml を編集してから起動
./clipsync-server --config config.yaml
```

---

## ⚙️ 設定

すべてのデフォルト値は [config.default.yaml](config.default.yaml) に記述され、`go:embed` でバイナリに同梱されます。**Go コードにハードコードされたデフォルトはありません。**

```bash
# コメント付きの完全な YAML を出力して起点にする
./clipsync-server --print-default-config > config.yaml
```

設定ファイルの検索順: `--config` フラグ → `CLIPSYNC_CONFIG` 環境変数 → `./config.yaml` → `/etc/clipsync/config.yaml`。ファイルが存在しなくてもエラーにはならず、内蔵デフォルトが使用されます。

| 項目 | 説明 | デフォルト |
|---|---|---|
| `server.addr` | HTTP/WS 待受アドレス | `:28001` |
| `server.read_timeout` / `write_timeout` | HTTP 読み書きタイムアウト | `15s` |
| `server.shutdown_timeout` | グレースフルシャットダウン待機時間 | `10s` |
| `server.trust_proxy` | XFF/XRI プロキシヘッダーを信頼 | `false` |
| `server.admin_token` | `/server-admin/*` の Bearer トークン。空欄ならエンドポイントを無効化 | 空 |
| `logs.dir` / `level` / `stdout` / `max_age_days` | ログディレクトリ / レベル / stdout 出力 / 保持日数 | `logs` / `info` / `true` / `0` |
| `websocket.read_limit` | 1メッセージの最大サイズ（クリップボード画像は大きくなりがち） | `10485760` (10MB) |
| `websocket.ping_interval_sec` | Ping 間隔 | `30` |
| `websocket.send_queue_size` | クライアントごとの送信キュー長 | `32` |
| `message_protocol.max_payload_preview` | ログ上のプレビュー文字数 | `40` |
| `mysql.*` | MySQL 接続（DSN は自動構築） | `127.0.0.1:3306/clipsync` |
| `redis.addr` / `db` / `key_prefix` / `online_ttl_sec` | Redis アドレス / DB / キープレフィックス / オンライン TTL | `127.0.0.1:6379` / `0` / `clipsync:` / `90` |
| `auth.token_ttl_hours` | トークン有効期間（時間） | `720`（30日） |
| `auth.allow_register` | `POST /auth/register` を公開するか | `false` |
| `auth.min_password_len` | 登録時の最小パスワード長 | `8` |
| `auth.bootstrap_user/password` | 起動時に自動作成する初期アカウント | 空 |
| `auth.login_rate_limit_per_min` | IP ごとの1分あたりログイン試行上限 | `10` |
| `e2ee.require` | 平文メッセージを拒否し `/push` を無効化 | `false` |

環境変数でオーバーライドできるのは以下の少数の項目のみ: `CLIPSYNC_ADDR`, `CLIPSYNC_LOG_DIR`, `CLIPSYNC_LOG_LEVEL`, `CLIPSYNC_TRUST_PROXY`, `CLIPSYNC_WS_READ_LIMIT`, `CLIPSYNC_TOKEN_TTL_HOURS`, `CLIPSYNC_ALLOW_REGISTER`, `CLIPSYNC_BOOTSTRAP_USER`, `CLIPSYNC_BOOTSTRAP_PASSWORD`, `CLIPSYNC_E2EE_REQUIRE`。

> ⚠️ **MySQL/Redis の接続情報は設定ファイルからのみ読み込まれます**（環境変数オーバーライドなし）。これは古い環境変数が `config.yaml` を知らない間に上書きする事故を防ぐためです。変更後はプロセスの再起動が必要で、ホットリロードには対応していません。

---

## 🔌 API リファレンス

### クライアント向けエンドポイント

| パス | メソッド | 説明 |
|---|---|---|
| `/ws?token=&device=&role=pc\|mobile&platform=&caps=&name=` | GET (WS) | 長寿命クライアント接続。ハンドシェイク時に認証と端末審査を実施 |
| `/auth/register` | POST | 登録（`allow_register` で制御） |
| `/auth/login` | POST | ユーザー名/パスワードをトークンに交換 |
| `/auth/session` | GET | 現在のセッションとオンライン端末を取得 |
| `/auth/logout` | POST | ログアウトしてセッションを消去 |
| `/auth/change-password` | POST | 自身のパスワードを変更 |
| `/device/name` | POST | 現在の端末のカスタム名を変更 |
| `/push?token=` | POST (JSON) | 簡易プッシュ（curl でテスト可能、`e2ee.require` 有効時は無効） |
| `/health` | GET | ヘルスチェック |

### 管理 API（`/server-admin/*`、Bearer トークン）

| パス | メソッド | 説明 |
|---|---|---|
| `/server-admin/users` | POST | 管理者によるユーザー作成（登録スイッチをバイパス） |
| `/server-admin/users/{id}/devices` | GET | ユーザーの全端末を取得（オンライン状態はメモリ Hub 準拠） |
| `/server-admin/users/{id}/devices/{deviceID}/status` | PUT | 端末の有効/無効化（無効化すると同時にキック） |
| `/server-admin/users/{id}/devices/{deviceID}/name` | PUT | 端末名変更（presence をブロードキャスト） |
| `/server-admin/devices` | GET | 全ユーザー横断のページング検索（keyword/disabled/user_id） |
| `/server-admin/kick` | POST | 統一キックエントリ（kick_user / kick_device / disable_device / enable_device） |

### メッセージフレーム例

```json
{ "type": "notify_pc", "kind": "sms_code", "text": "【MyBank】認証コード 314159" }
```

```bash
# SMS コードを全 PC クライアントへプッシュ
curl -X POST 'http://127.0.0.1:28001/push?token=<your-token>' \
  -H 'Content-Type: application/json' \
  -d '{"type":"notify_pc","kind":"sms_code","text":"【テスト】認証コードは 314159"}'
```

---

## 🔐 プレゼンスモデル

```
       ┌──── メモリ Hub（権威）────┐
       │  userID → set<*Client>    │
       └───────────┬───────────────┘
                   │ 30秒ごとのハートビートで更新
                   ▼
       Redis Hash  clipsync:online:<userID>
                 field=deviceID  value=role
                 TTL = 90秒（TTL/3 間隔でリフレッシュ）
```

- **オンライン判定はメモリ Hub が権威**です。接続があればオンライン、なければオフライン。
- Redis Hash は主にログイン時の「このユーザーには既にクライアントがあるか」の判定や、管理画面でのクロスプロセス表示に使われます。
- `kill -9` でプロセスが落ちても、Redis のエントリは90秒以内に自然に失効し、幽霊オンラインは残りません。

---

## 🐳 デプロイ構成

### ClipSync-Admin との連携

```
        ┌────────────────────┐        Redis Pub/Sub
        │  ClipSync-Admin    │ ───────────────────────▶  ClipSync-Server
        │  (admin :28002)    │   clipsync:admin:kick_user  (本サービス :28001)
        └─────────┬──────────┘ ◀───────────────────────  └────────┬─────────┘
                  │  HTTP フォールバック (admin_token)             │
                  └───────────────────────────────────────────────┘
                          同一 MySQL（clipsync DB）を共有
```

- 両サービスは同一の MySQL データベース `clipsync` を共有します;
- Server が `users` / `sessions` / `devices` テーブルの書き込み権限を持ちます;
- Admin は Redis Pub/Sub 経由で Server にキック/無効化を通知し、Redis 不通時は HTTP にフォールバックします;
- 端末のオンライン状態はまず Server の `/server-admin/users/{id}/devices` から取得し、失敗時はローカル MySQL+Redis にフォールバックします。

### リバースプロキシ

本番では必ず Nginx/Caddy + TLS を前面に置いてください。[Caddyfile.example](Caddyfile.example) と [deploy/nginx.clipsync.conf](deploy/nginx.clipsync.conf)（Admin リポジトリにありますが、プロキシ設定は Server にも共通）を参照してください。

重要: **WebSocket の長寿命接続には長い `proxy_read_timeout`** が必要です（3600秒以上を推奨）。実際のクライアント IP を記録するため、`server.trust_proxy: true` も必要です。

```nginx
location = /clipsync/ws {
    proxy_pass http://127.0.0.1:28001/ws;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_read_timeout 3600s;
}
```

---

## 📁 プロジェクト構成

```
ClipSync-Server/
├── main.go                 # ルーティング + WebSocket Hub + メッセージルーティング + グレースフルシャットダウン
├── config.go               # 設定構造体 + YAML 読み込み + 環境変数オーバーライド
├── config.default.yaml     # 同梱デフォルト設定（デフォルト値の唯一のソース）
├── auth_http.go            # /auth/* 登録/ログイン/セッション/パスワード変更 HTTP
├── auth_service.go         # 認証ロジック: ログイン、セッション再利用、レート制限
├── auth_crypto.go          # scrypt パスワードハッシュ + トークン SHA-256
├── admin_http.go           # /server-admin/* 管理 API
├── device_name_http.go     # セルフ端末リネーム
├── store_mysql.go          # MySQL: users / sessions テーブル
├── store_device.go         # MySQL: devices テーブル（upsert/無効化/リネーム/検索）
├── store_redis.go          # Redis: トークンキャッシュ + オンライン Hash + Pub/Sub
├── e2ee.go                 # E2EE ゲート + エンベロープ解析
├── logger.go               # 日次ローテートロガー（汎用 + メッセージカテゴリ）
├── Dockerfile              # マルチステージビルド → distroless nonroot
├── docker-compose.yml      # ホストネットワーク構成
├── Caddyfile.example       # HTTPS リバースプロキシ例
├── deploy/
│   ├── config.compose.yaml   # オールイン Docker 用の設定雛形
│   ├── config.external.yaml  # MySQL/Redis がホスト上にある場合の設定雛形
│   └── mysql/init.sql        # DB 初期化スクリプト（サーバー起動時にも自動マイグレーション）
└── .github/workflows/docker-image.yml  # タグ push でマルチアーキイメージを自動ビルド
```

---

## 🔐 セキュリティ

| 観点 | 設計 |
|------|------|
| パスワード保存 | scrypt（N=32768, r=8, p=1、32バイト派生鍵、16バイトランダムソルト） |
| トークン保存 | MySQL には SHA-256 ハッシュのみ保存。平文トークンは TTL 付きで Redis に置くだけで、DB 漏洩時も再利用不可 |
| 通信暗号化 | Nginx/Caddy で `wss://` を構成。E2EE は AES-256-GCM をクライアント側で実施 |
| ブルートフォース対策 | IP ごとにログイン試行を制限（デフォルト10回/分） |
| 管理 API | `admin_token` を `crypto/subtle.ConstantTimeCompare` で比較しタイミング攻撃を防止 |
| クライアント偽装 | サーバーが `from` フィールドを強制上書き。`ping/pong/presence` 制御メッセージは転送しない |
| イメージ強化 | distroless nonroot（uid 65532）、シェルなし・パッケージマネージャなしで最小の攻撃面 |

---

## 🐛 トラブルシューティング

| 現象 | 確認すること |
|------|--------------|
| クライアントが接続できない | ファイアウォールで 28001 を許可、プロキシが Upgrade ヘッダーを転送しているか、URL は `ws://IP:28001/ws` |
| ログの IP がプロキシのアドレスばかり | `server.trust_proxy: true` を有効化して再起動 |
| 管理 API が 401 | `server.admin_token` が設定されているか、`Authorization: Bearer <token>` が送られているか |
| コンテナ再起動でログが消える | `./logs:/data/logs` をマウントしているか |
| 設定変更が反映されない | 設定は起動時に読み込み。`docker compose restart` または `systemctl restart` が必要 |
| 切断済み端末が Redis でオンライン表示 | 90秒の TTL で自然失効。実際のルーティングにはメモリ Hub が使われ影響なし |
| `/push` が 403 を返す | `e2ee.require=true` の場合、平文プッシュは無効化されます |

ログ出力先: `logs/clipsync.log`（汎用）、`logs/message.log`（メッセージ中継監査）、日次アーカイブは `logs/clipsync/`・`logs/message/`。

---

## 🤝 関連プロジェクト

| プロジェクト | 技術スタック | リンク |
|------|--------|------|
| 管理画面バックエンド | Go + Gin + GORM | [JH-Clipsync/ClipSync-Admin](https://github.com/JH-Clipsync/ClipSync-Admin) |
| 管理画面フロントエンド | Vue 3 + Element Plus | [JH-Clipsync/ClipSync-Admin-Web](https://github.com/JH-Clipsync/ClipSync-Admin-Web) |
| Android クライアント | Kotlin + OkHttp | [JH-Clipsync/ClipSync-Android](https://github.com/JH-Clipsync/ClipSync-Android) |
| macOS クライアント | Swift + SwiftUI | [JH-Clipsync/ClipSync-Mac](https://github.com/JH-Clipsync/ClipSync-Mac) |
| Windows クライアント | .NET 8 + WPF | [JH-Clipsync/ClipSync-Windows](https://github.com/JH-Clipsync/ClipSync-Windows) |

---

## 📄 License

個人利用のプロジェクトです。コードの参照・改変は自由に行ってください。

---

**Made with ❤️ · 全プラットフォーム自作 · データはあなたのもの**
