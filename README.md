# isucon14-SelfChronicle

## 2024
- **2024-12-08**: isucon14

## 本戦実施内容
- DBにINDEXを付与
- APP, DB分割
  - DB設定のbind-addressをlisten可に変更後、env.shで接続先を変更、DB接続は出来るがbenchmarkでmatching処理に失敗する
  - 原因はisuride-matcherのprocessが1つである必要があるため
  - isuride-matcherが(/api/internal/matching:internalGetMatching)を叩いている事が原因、daemonを止める必要があった

## 反省会
- [公式反省会](https://lycorptech-jp.connpass.com/event/340046/)
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

このあたりの改良を加えることで、75000 程度のスコアが出ることを確認した


```

