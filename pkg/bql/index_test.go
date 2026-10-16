package bql

import (
	"strings"
	"testing"

	"basaldb/pkg/engine"
)

func TestParseCreateIndex(t *testing.T) {
	stmt, err := Parse(`CREATE INDEX ON Pages(url)`)
	if err != nil {
		t.Fatal(err)
	}
	ix, ok := stmt.(*CreateIndexStmt)
	if !ok || ix.Layer != "Pages" || ix.Attr != "url" {
		t.Fatalf("stmt = %#v", stmt)
	}
	if _, err := Parse(`CREATE INDEX Pages(url)`); err == nil {
		t.Fatal("missing ON parsed")
	}
}

func TestIndexedGetUpdateDeleteAndSchema(t *testing.T) {
	dir := t.TempDir()
	layers, err := engine.ParseLayers("0:Accounts(name!,plan),1:Users(email,role!)")
	if err != nil {
		t.Fatal(err)
	}
	db, err := engine.Init(dir+"/ix.bsl", layers, engine.TxMeta{Author: "t", Summary: "s", Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := &ExecContext{DB: db, Meta: engine.TxMeta{Author: "t", Summary: "s", Reason: "r"}}

	if _, err := Exec(ctx, `CREATE Accounts [name="Acme",plan="pro"]`); err != nil {
		t.Fatal(err)
	}
	if _, err := Exec(ctx, `CREATE Users PARENT Accounts[name="Acme"] [email="a@x",role="admin"]`); err != nil {
		t.Fatal(err)
	}
	if _, err := Exec(ctx, `CREATE Users PARENT Accounts[name="Acme"] [email="b@x",role="member"]`); err != nil {
		t.Fatal(err)
	}
	res, err := Exec(ctx, `GET Users[role="admin"]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0].Attrs[0] != "a@x" {
		t.Fatalf("indexed get = %+v", res.Rows)
	}
	res, err = Exec(ctx, `GET Accounts[name="Acme"][plan="pro"]`)
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("filtered index get = %+v err=%v", res, err)
	}
	res, err = Exec(ctx, `SHOW SCHEMA Accounts`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Columns) != 3 || !strings.Contains(res.Rows[0].Cells[2], "name!") {
		t.Fatalf("schema = %+v", res.Rows)
	}

	if _, err := Exec(ctx, `UPDATE Users[role="admin"][role="owner"]`); err != nil {
		t.Fatal(err)
	}
	res, err = Exec(ctx, `GET Users[role="admin"]`)
	if err != nil || len(res.Rows) != 0 {
		t.Fatalf("old role rows = %+v err=%v", res, err)
	}
	res, err = Exec(ctx, `GET Users[role="owner"]`)
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("new role rows = %+v err=%v", res, err)
	}
	if _, err := Exec(ctx, `DELETE Accounts[name="Acme"]`); err != nil {
		t.Fatal(err)
	}
	res, err = Exec(ctx, `GET Users[role="owner"]`)
	if err != nil || len(res.Rows) != 0 {
		t.Fatalf("after cascade = %+v err=%v", res, err)
	}

	if _, err := Exec(ctx, `CREATE Accounts [name="Beta",plan="free"]`); err != nil {
		t.Fatal(err)
	}
	if _, err := Exec(ctx, `CREATE INDEX ON Accounts(plan)`); err != nil {
		t.Fatal(err)
	}
	res, err = Exec(ctx, `SHOW SCHEMA`)
	if err != nil {
		t.Fatal(err)
	}
	var acct string
	for _, row := range res.Rows {
		if row.Cells[1] == "Accounts" {
			acct = row.Cells[2]
		}
	}
	if !strings.Contains(acct, "name!") || !strings.Contains(acct, "plan!") {
		t.Fatalf("schema after CREATE INDEX = %q", acct)
	}
	res, err = Exec(ctx, `GET Accounts[plan="free"]`)
	if err != nil || len(res.Rows) != 1 || res.Rows[0].Attrs[0] != "Beta" {
		t.Fatalf("plan index get = %+v err=%v", res, err)
	}
}
