# isucon14-SelfChronicle

## 2024
- **2024-12-08**: isucon14

## 本戦実施内容
- DBにINDEXを付与した
- APP, DB分割するも競技中は原因を特定できなかった
 - DBのbind-addressを設定変更後、env.shで接続先を変更、DB接続は出来るがbenchmarkでmatching処理に失敗する
 - isuride-matcherが(/api/internal/matching:internalGetMatching)を叩いている事が原因、daemonを止める必要があった

## 備考
