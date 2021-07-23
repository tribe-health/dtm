package dtmcli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"github.com/yedf/dtm/common"
)

// BusiFunc type for busi func
type BusiFunc func(db *sql.DB) (interface{}, error)

// TransInfo every branch info
type TransInfo struct {
	TransType  string
	Gid        string
	BranchID   string
	BranchType string
}

func (t *TransInfo) String() string {
	return fmt.Sprintf("transInfo: %s %s %s %s", t.TransType, t.Gid, t.BranchID, t.BranchType)
}

// TransInfoFromReq construct transaction info from request
func TransInfoFromReq(c *gin.Context) *TransInfo {
	ti := &TransInfo{
		TransType:  c.Query("trans_type"),
		Gid:        c.Query("gid"),
		BranchID:   c.Query("branch_id"),
		BranchType: c.Query("branch_type"),
	}
	if ti.TransType == "" || ti.Gid == "" || ti.BranchID == "" || ti.BranchType == "" {
		panic(fmt.Errorf("invlid trans info: %v", ti))
	}
	return ti
}

// BarrierModel barrier model for gorm
type BarrierModel struct {
	common.ModelBase
	TransInfo
}

func logExec(tx *sql.Tx, query string, args ...interface{}) (sql.Result, error) {
	logrus.Printf("executing: "+query, args...)
	return tx.Exec(query, args...)
}

func logQueryRow(tx *sql.Tx, query string, args ...interface{}) *sql.Row {
	logrus.Printf("querying: "+query, args...)
	return tx.QueryRow(query, args...)
}

// TableName gorm table name
func (BarrierModel) TableName() string { return "dtm_barrier.barrier" }

func insertBarrier(tx *sql.Tx, transType string, gid string, branchID string, branchType string, reason string) (int64, error) {
	if branchType == "" {
		return 0, nil
	}
	res, err := logExec(tx, "insert ignore into dtm_barrier.barrier(trans_type, gid, branch_id, branch_type, reason) values(?,?,?,?,?)", transType, gid, branchID, branchType, reason)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ThroughBarrierCall 子事务屏障，详细介绍见 https://zhuanlan.zhihu.com/p/388444465
// db: 本地数据库
// transInfo: 事务信息
// bisiCall: 业务函数，仅在必要时被调用
// 返回值:
// 如果正常调用，返回bisiCall的结果
// 如果发生重复调用，则busiCall不会被重复调用，直接对保存在数据库中上一次的结果，进行unmarshal，通常是一个map[string]interface{}，直接作为http的resp
// 如果发生悬挂，则busiCall不会被调用，直接返回错误 {"dtm_result": "FAILURE"}
// 如果发生空补偿，则busiCall不会被调用，直接返回 {"dtm_result": "SUCCESS"}
func ThroughBarrierCall(db *sql.DB, transInfo *TransInfo, busiCall BusiFunc) (res interface{}, rerr error) {
	tx, rerr := db.BeginTx(context.Background(), &sql.TxOptions{})
	if rerr != nil {
		return
	}
	defer func() {
		logrus.Printf("result is %v error is %v", res, rerr)
		if x := recover(); x != nil {
			tx.Rollback()
			panic(x)
		} else if rerr != nil {
			tx.Rollback()
		} else {
			tx.Commit()
		}
	}()
	ti := transInfo
	originType := map[string]string{
		"cancel":     "try",
		"compensate": "action",
	}[ti.BranchType]
	originAffected, _ := insertBarrier(tx, ti.TransType, ti.Gid, ti.BranchID, originType, ti.BranchType)
	currentAffected, rerr := insertBarrier(tx, ti.TransType, ti.Gid, ti.BranchID, ti.BranchType, ti.BranchType)
	logrus.Printf("originAffected: %d currentAffected: %d", originAffected, currentAffected)
	if (ti.BranchType == "cancel" || ti.BranchType == "compensate") && originAffected > 0 { // 这个是空补偿，返回成功
		res = common.MS{"dtm_result": "SUCCESS"}
		return
	} else if currentAffected == 0 { // 插入不成功
		var result sql.NullString
		err := logQueryRow(tx, "select result from dtm_barrier.barrier where trans_type=? and gid=? and branch_id=? and branch_type=? and reason=?",
			ti.TransType, ti.Gid, ti.BranchID, ti.BranchType, ti.BranchType).Scan(&result)
		if err == sql.ErrNoRows { // 这个是悬挂操作，返回失败，AP收到这个返回，会尽快回滚
			res = common.MS{"dtm_result": "FAILURE"}
			return
		}
		if err != nil {
			rerr = err
			return
		}
		if result.Valid { // 数据库里有上一次结果，返回上一次的结果
			res = json.Unmarshal([]byte(result.String), &res)
			return
		}
		// 数据库里没有上次的结果，属于重复空补偿，直接返回成功
		res = common.MS{"dtm_result": "SUCCESS"}
		return
	}
	res, rerr = busiCall(db)
	if rerr == nil { // 正确返回了，需要将结果保存到数据库
		sval := common.MustMarshalString(res)
		_, rerr = logExec(tx, "update dtm_barrier.barrier set result=? where trans_type=? and gid=? and branch_id=? and branch_type=?", sval,
			ti.TransType, ti.Gid, ti.BranchID, ti.BranchType)
	}
	return
}
