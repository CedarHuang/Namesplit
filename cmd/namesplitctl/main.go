// Command namesplitctl 是开发用的配置与规则命令行工具。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/miekg/dns"

	"github.com/CedarHuang/Namesplit/internal/api"
	"github.com/CedarHuang/Namesplit/internal/config"
	"github.com/CedarHuang/Namesplit/internal/diagnostic"
	"github.com/CedarHuang/Namesplit/internal/dnsname"
	"github.com/CedarHuang/Namesplit/internal/localrecords"
	"github.com/CedarHuang/Namesplit/internal/rules"
)

func main() {
	if len(os.Args) < 3 {
		usage()

		os.Exit(2)
	}

	switch os.Args[1] + " " + os.Args[2] {
	case "config validate":
		os.Exit(runConfigValidate(os.Args[3:]))
	case "rules test":
		os.Exit(runRulesTest(os.Args[3:]))
	default:
		usage()

		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `用法:
  namesplitctl config validate --file <config.toml>
  namesplitctl rules test --file <config.toml> --domain <name> [--qtype A]`)
}

// runConfigValidate 校验配置文件。
func runConfigValidate(args []string) int {
	flags := flag.NewFlagSet("config validate", flag.ContinueOnError)
	file := flags.String("file", "", "配置文件路径")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *file == "" {
		fmt.Fprintln(os.Stderr, "缺少 --file")

		return 2
	}

	data, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取配置失败: %v\n", err)

		return 1
	}

	_, diags := config.ParseFile(data)
	errorsOnly := diagnostic.Filter(diags, diagnostic.SeverityError)
	warnings := diagnostic.Filter(diags, diagnostic.SeverityWarning)

	writeJSON(api.Validation{Valid: len(errorsOnly) == 0, Errors: errorsOnly, Warnings: warnings})
	if len(errorsOnly) > 0 {
		return 1
	}

	return 0
}

// runRulesTest 测试域名命中的本地记录与规则。
func runRulesTest(args []string) int {
	flags := flag.NewFlagSet("rules test", flag.ContinueOnError)
	file := flags.String("file", "", "配置文件路径")
	domain := flags.String("domain", "", "待测试域名")
	qtypeText := flags.String("qtype", "A", "查询类型，仅展示")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *file == "" || *domain == "" {
		fmt.Fprintln(os.Stderr, "缺少 --file 或 --domain")

		return 2
	}

	data, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取配置失败: %v\n", err)

		return 1
	}

	cfg, diags := config.ParseFile(data)
	if diagnostic.HasError(diags) {
		writeJSON(api.Validation{Valid: false, Errors: diagnostic.Filter(diags, diagnostic.SeverityError)})

		return 1
	}

	canon, err := dnsname.CanonQuery(*domain)
	if err != nil {
		fmt.Fprintf(os.Stderr, "域名非法: %v\n", err)

		return 1
	}

	qtype := parseQType(*qtypeText)

	result := api.RuleTest{
		Canonical:    canon,
		DefaultGroup: cfg.Settings.DefaultGroup,
		Warnings:     diagnostic.Filter(diags, diagnostic.SeverityWarning),
	}

	ruleInputs := make([]rules.Rule, 0, len(cfg.Rules))
	for _, rule := range cfg.Rules {
		ruleInputs = append(ruleInputs, rules.Rule{
			Kind:   rules.Kind(rule.Match),
			Domain: rule.Domain,
			Group:  rule.Group,
		})
	}
	rulesTable, _ := rules.Compile(ruleInputs)
	if hit, ok := rulesTable.Match(*domain); ok {
		result.Rule = &api.RuleTestRule{
			Index:  hit.Index,
			Match:  string(hit.Kind),
			Domain: hit.Domain,
			Group:  hit.Group,
		}
		result.TargetGroup = hit.Group
	}
	if result.TargetGroup == "" {
		result.TargetGroup = cfg.Settings.DefaultGroup
	}

	recordInputs := make([]localrecords.Record, 0, len(cfg.LocalRecords))
	for _, record := range cfg.LocalRecords {
		recordInputs = append(recordInputs, localrecords.Record{
			Type:    localrecords.Type(record.Type),
			Name:    record.Name,
			Value:   record.Value,
			TTL:     record.TTL,
			Enabled: record.Enabled,
		})
	}
	localTable, _ := localrecords.Compile(recordInputs, cfg.Settings.PtrAuto)
	if local, ok := localTable.Lookup(*domain, qtype); ok {
		answers := make([]string, 0, len(local.Answers))
		var ttl uint32
		for index, rr := range local.Answers {
			answers = append(answers, rr.String())
			if index == 0 || rr.Header().Ttl < ttl {
				ttl = rr.Header().Ttl
			}
		}

		result.Local = api.RuleTestLocal{
			Hit:     true,
			Name:    canon,
			QType:   qtypeName(qtype),
			NODATA:  local.NODATA,
			Answers: answers,
			TTL:     ttl,
		}
	}

	writeJSON(result)

	return 0
}

func writeJSON(value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "序列化结果失败: %v\n", err)

		return
	}

	fmt.Println(string(data))
}

func parseQType(text string) uint16 {
	trimmed := strings.ToUpper(strings.TrimSpace(text))
	if trimmed == "" {
		return dns.TypeA
	}
	if qtype, ok := dns.StringToType[trimmed]; ok {
		return qtype
	}

	return dns.TypeA
}

func qtypeName(qtype uint16) string {
	if name, ok := dns.TypeToString[qtype]; ok {
		return name
	}

	return fmt.Sprintf("TYPE%d", qtype)
}
