# isucon14-SelfChronicle

## 2024
- **2024-12-08**: isucon14

## 本戦実施内容
- DBにINDEXを付与
- APP, DB分割
  - DB設定のbind-addressをlisten可に変更後、env.shで接続先を変更、DB接続は出来るがbenchmarkでmatching処理に失敗する
  - 原因はisuride-matcherのprocessが1つである必要があるため
  - isuride-matcherが(/api/internal/matching:internalGetMatching)を叩いている事が原因、daemonを止める必要があった

## 備考
