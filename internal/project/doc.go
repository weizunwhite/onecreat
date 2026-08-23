// Package project 是科创项目实体的领域包。
//
// 它管三件事：
//
//   - project.json 的**保序**读写（projectjson.go）——项目根下唯一允许的散文件，
//     同时被 Mac 侧四个 Python 外部脚本读写，格式必须逐字节兼容；
//   - 在研项目目录的枚举（scan.go）；
//   - 七目录判例（bucket.go）——某份材料该落进哪个桶。
//
// 边界（架构铁律，见 docs/科创工作流迁移/00_架构规划_v1.md §3.2）：
// 本包属于 L4 领域层，只依赖标准库与同层的 internal/workflow/registry。
// 它**不得**被 internal/engine、internal/engine/dsh、internal/agent import
// （internal/engine/boundary_test.go 与 internal/agent/policy_boundary_test.go
// 会因此报红）；反过来它也不认识 control / toolpolicy / tool / plugin。
//
// 本包不搬目录、不改名：换业务线只能走外部的 reline_project.py，
// 项目实体目录永不搬家、永不手改名。
package project
