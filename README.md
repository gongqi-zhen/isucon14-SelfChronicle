
# isucon14-SelfChronicle

ゆるっと
![スクリーンショット 2025-01-13 2 48 48](https://github.com/user-attachments/assets/f0a5eaac-9ce4-4453-bfba-82db491b6c2f)

## 2024
- **2024-12-08**: isucon14
- **2025-01-17**: 感想戦モード2025年1月17日16時まで

## 本戦内容
- DBにINDEXを付与
- APP, DB分割
  - DB設定のbind-addressをlisten可に変更後、env.shで接続先を変更、DB接続は出来るがbenchmarkでmatching処理に失敗する
  - 原因はisuride-matcherのprocessが1つである必要があるため
  - isuride-matcherが(/api/internal/matching:internalGetMatching)を叩いている事が原因、daemonを止める必要があった

## 反省会
- [公式反省会](https://lycorptech-jp.connpass.com/event/340046/)

```
制限時間: 8時間
アプリ仕様・サービス内容を理解しし
何をするのか・どんな仕組みかを理解し、もっと効率良く出来ないかを競う競技である。

皆さん理解度が早く・手が動く
正直なところ、競技中にアプリが何をするのか・どんな仕組みか理解も理解が出来ていなかった。

感想戦モードを通して
何度もアプリケーションマニュアルを読み直し、動いている初期状態のコードを読み・やっと構成の理解が深まってくる。
```

## [感想戦モード](https://discord.com/channels/1281221321166163990/1281221321174421572/1316656816603533362)

ベンチマーク実行

https://github.com/user-attachments/assets/a11b8cdc-2805-4dea-953c-2584d14b375d


### 実施内容
```
・適宜index付与
・internalGetMatchingのmatching呼び出し一度で全てマッチさせる
・chairの全権取得(*)を止めて必要なカラムを取得し、is_activeで絞り込む、不要なselectを止める
・chairに位置情報(latitude, longitude)を持たせる,初期化処理(postInitialize)でDBも初期化しているので、chair_locationsから情報を取得してchairに入れる
・matching構造体を定義してそこに距離を持たせる、chairとrideの距離を先に計算して距離が小さい順に並べておく、距離が遠すぎる場合はマッチングせずcontinueする。
・chairにtotal_distance, moved_atを持たせる
・appGetNearbyChairsを改善する
・multihostにする (DBサーバの設定を他のサーバから接続できるようにする。そしてisuride-matcherを止める。)
・DBの負荷が高いこと、matchingの確認頻度に併せて、notificationのRetryAfterMs設定を30 -> 400程度の調整する

[ToDos]
追記する...

```

