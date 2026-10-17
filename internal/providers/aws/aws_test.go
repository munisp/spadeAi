package aws

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/munisp/spadeai/internal/providers/cloud/cloudtest"
)

type inst struct {
	id, state string
	tags      map[string]string
	token     string
}

// fakeEC2 implements the EC2 query API calls the driver uses, one instance per DescribeInstances page.
func fakeEC2(t *testing.T) *httptest.Server {
	var mu sync.Mutex
	var all []*inst
	xmlErr := func(w http.ResponseWriter, code int, c string) {
		w.WriteHeader(code)
		fmt.Fprintf(w, `<Response><Errors><Error><Code>%s</Code><Message>%s</Message></Error></Errors><RequestID>r</RequestID></Response>`, c, c)
	}
	item := func(i *inst) string {
		var tags strings.Builder
		for k, v := range i.tags {
			fmt.Fprintf(&tags, "<item><key>%s</key><value>%s</value></item>", k, v)
		}
		return fmt.Sprintf(`<item><instanceId>%s</instanceId><instanceState><code>16</code><name>%s</name></instanceState><ipAddress>203.0.113.9</ipAddress><tagSet>%s</tagSet></item>`, i.id, i.state, tags.String())
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=AKIAGOOD/") {
			xmlErr(w, 401, "AuthFailure")
			return
		}
		_ = r.ParseForm()
		f := r.Form
		w.Header().Set("Content-Type", "text/xml")
		switch f.Get("Action") {
		case "DescribeAvailabilityZones":
			fmt.Fprint(w, `<DescribeAvailabilityZonesResponse><requestId>r</requestId><availabilityZoneInfo/></DescribeAvailabilityZonesResponse>`)
		case "DescribeInstances":
			var match []*inst
			for _, i := range all {
				ok := true
				for n := 1; f.Get(fmt.Sprintf("Filter.%d.Name", n)) != ""; n++ {
					name := f.Get(fmt.Sprintf("Filter.%d.Name", n))
					var vals []string
					for v := 1; f.Get(fmt.Sprintf("Filter.%d.Value.%d", n, v)) != ""; v++ {
						vals = append(vals, f.Get(fmt.Sprintf("Filter.%d.Value.%d", n, v)))
					}
					switch {
					case name == "instance-state-name":
						ok = ok && slices.Contains(vals, i.state)
					case strings.HasPrefix(name, "tag:"):
						ok = ok && slices.Contains(vals, i.tags[strings.TrimPrefix(name, "tag:")])
					default:
						t.Errorf("unexpected filter %s", name)
					}
				}
				if ok {
					match = append(match, i)
				}
			}
			n, _ := strconv.Atoi(f.Get("NextToken"))
			body, next := "", ""
			if n < len(match) {
				body = item(match[n])
				if n+1 < len(match) {
					next = "<nextToken>" + strconv.Itoa(n+1) + "</nextToken>"
				}
			}
			fmt.Fprintf(w, `<DescribeInstancesResponse><requestId>r</requestId><reservationSet><item><reservationId>r-1</reservationId><instancesSet>%s</instancesSet></item></reservationSet>%s</DescribeInstancesResponse>`, body, next)
		case "RunInstances":
			ud, _ := base64.StdEncoding.DecodeString(f.Get("UserData"))
			if f.Get("ImageId") != "ami-ubuntu" || f.Get("InstanceType") != "aws-size" || f.Get("MetadataOptions.HttpTokens") != "required" ||
				!strings.HasPrefix(string(ud), "#cloud-config") || f.Get("TagSpecification.2.ResourceType") != "volume" {
				t.Errorf("bad RunInstances: %v", f)
			}
			for _, i := range all {
				if i.token == f.Get("ClientToken") {
					fmt.Fprintf(w, `<RunInstancesResponse><reservationId>r-1</reservationId><instancesSet>%s</instancesSet></RunInstancesResponse>`, item(i))
					return
				}
			}
			i := &inst{id: fmt.Sprintf("i-%04d", len(all)+1), state: "pending", tags: map[string]string{}, token: f.Get("ClientToken")}
			for n := 1; f.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Key", n)) != ""; n++ {
				i.tags[f.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Key", n))] = f.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Value", n))
			}
			all = append(all, i)
			fmt.Fprintf(w, `<RunInstancesResponse><reservationId>r-1</reservationId><instancesSet>%s</instancesSet></RunInstancesResponse>`, item(i))
		case "TerminateInstances":
			for _, i := range all {
				if i.id == f.Get("InstanceId.1") {
					i.state = "terminated"
					fmt.Fprintf(w, `<TerminateInstancesResponse><requestId>r</requestId><instancesSet><item><instanceId>%s</instanceId><currentState><name>shutting-down</name></currentState></item></instancesSet></TerminateInstancesResponse>`, i.id)
					return
				}
			}
			xmlErr(w, 400, "InvalidInstanceID.NotFound")
		default:
			t.Errorf("unexpected action %q", f.Get("Action"))
			xmlErr(w, 400, "InvalidAction")
		}
	}))
}

func TestContract(t *testing.T) {
	srv := fakeEC2(t)
	defer srv.Close()
	mk := func(key string) *Client {
		return &Client{AccessKeyID: key, SecretAccessKey: "secret", AMI: "ami-ubuntu", BaseURL: srv.URL}
	}
	cloudtest.Contract(t, mk("AKIAGOOD"), mk("AKIABAD"), &Client{BaseURL: srv.URL, AMI: "ami-ubuntu"})
}

func TestDestroyMissingIsNoop(t *testing.T) {
	srv := fakeEC2(t)
	defer srv.Close()
	c := &Client{AccessKeyID: "AKIAGOOD", SecretAccessKey: "s", AMI: "ami-ubuntu", BaseURL: srv.URL}
	if err := c.Destroy(t.Context(), "i-404"); err != nil {
		t.Fatalf("destroying a missing instance must succeed: %v", err)
	}
}
