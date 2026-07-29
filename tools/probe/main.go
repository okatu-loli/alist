package main

import (
	"context"
	"fmt"

	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/bootstrap"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

func main() {
	flags.DataDir = "/tmp/alist-v4-data"
	flags.MigrateMode = "off"
	bootstrap.InitConfig()
	bootstrap.InitDB()
	admin, err := op.GetAdmin()
	if err != nil {
		panic(err)
	}
	fmt.Printf("admin id=%d roles=%v\n", admin.ID, admin.Role)
	d, err := op.ResolveEffectivePolicy(admin, "/local")
	fmt.Printf("policy err=%v\n", err)
	if err == nil {
		fmt.Printf("can read=%v write=%v nopwd=%v traverse=%v\n",
			d.Can(model.ActRead), d.Can(model.ActWrite), d.Can("access_without_password"), d.Traverse)
	}
	authErr := op.Authorize(context.Background(), admin, model.ActRead, op.AuthzRequest{Path: "/local"})
	fmt.Printf("authorize read /local: %v\n", authErr)
}
