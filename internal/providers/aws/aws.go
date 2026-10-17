// Package aws drives EC2 instances tagged spade-service / spade-attempt. It only uses the SPADE_AWS_*
// credentials it is given, never the ambient AWS credential chain.
package aws

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/munisp/spadeai/internal/providers/cloud"
)

type Client struct {
	AccessKeyID, SecretAccessKey string
	Region                       string // default af-south-1 (Cape Town, the closest region to Nigeria)
	AMI                          string // Ubuntu 24.04 AMI id for Region; required, AMI ids are per region
	SubnetID                     string // optional; default VPC otherwise
	SecurityGroupIDs             []string
	BaseURL                      string // endpoint override (tests, gov/partner regions)
	HTTP                         *http.Client
}

func (c *Client) Name() string { return "aws" }
func (c *Client) TokenEnv() string {
	return "SPADE_AWS_ACCESS_KEY_ID/SPADE_AWS_SECRET_ACCESS_KEY/SPADE_AWS_AMI"
}
func (c *Client) Configured() bool {
	return c != nil && c.AccessKeyID != "" && c.SecretAccessKey != "" && c.AMI != ""
}
func (c *Client) InNigeria() bool { return false }

func (c *Client) ec2() (*ec2.Client, error) {
	if !c.Configured() {
		return nil, fmt.Errorf("aws: %s not configured", c.TokenEnv())
	}
	region := c.Region
	if region == "" {
		region = "af-south-1"
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	cfg := sdk.Config{Region: region, HTTPClient: hc,
		Credentials: sdk.NewCredentialsCache(credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, ""))}
	return ec2.NewFromConfig(cfg, func(o *ec2.Options) {
		if c.BaseURL != "" {
			o.BaseEndpoint = sdk.String(c.BaseURL)
		}
	}), nil
}

// live excludes shutting-down/terminated instances, which no longer run or bill.
var live = []string{"pending", "running", "stopping", "stopped"}

func server(i types.Instance, serviceID string) (cloud.Server, bool) {
	var svc, att, name string
	for _, t := range i.Tags {
		switch sdk.ToString(t.Key) {
		case "spade-service":
			svc = sdk.ToString(t.Value)
		case "spade-attempt":
			att = sdk.ToString(t.Value)
		case "Name":
			name = sdk.ToString(t.Value)
		}
	}
	var st string
	if i.State != nil {
		st = string(i.State.Name)
	}
	return cloud.Server{ID: sdk.ToString(i.InstanceId), Name: name, Status: st, IPv4: sdk.ToString(i.PublicIpAddress), Attempt: att}, svc == serviceID
}

func (c *Client) List(ctx context.Context, serviceID string) ([]cloud.Server, error) {
	cl, err := c.ec2()
	if err != nil {
		return nil, err
	}
	in := &ec2.DescribeInstancesInput{Filters: []types.Filter{
		{Name: sdk.String("tag:spade-service"), Values: []string{serviceID}},
		{Name: sdk.String("instance-state-name"), Values: live},
	}}
	var res []cloud.Server
	for p := ec2.NewDescribeInstancesPaginator(cl, in); p.HasMorePages(); {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("aws: %w", err)
		}
		for _, r := range page.Reservations {
			for _, i := range r.Instances {
				if s, ok := server(i, serviceID); ok {
					res = append(res, s)
				}
			}
		}
	}
	return res, nil
}

func (c *Client) Create(ctx context.Context, o cloud.CreateOpts) (*cloud.Server, error) {
	cl, err := c.ec2()
	if err != nil {
		return nil, err
	}
	tags := []types.Tag{
		{Key: sdk.String("Name"), Value: sdk.String(cloud.ServerName(o))},
		{Key: sdk.String("spade-service"), Value: sdk.String(o.ServiceID)},
		{Key: sdk.String("spade-attempt"), Value: sdk.String(o.Attempt)},
		{Key: sdk.String("spade-managed"), Value: sdk.String("true")},
	}
	in := &ec2.RunInstancesInput{
		ImageId: sdk.String(c.AMI), InstanceType: types.InstanceType(o.Size), MinCount: sdk.Int32(1), MaxCount: sdk.Int32(1),
		UserData: sdk.String(base64.StdEncoding.EncodeToString([]byte(o.UserData))),
		// EC2's own idempotency: a retried RunInstances with the same token returns the same instance.
		ClientToken:     sdk.String(o.ServiceID + "-" + o.Attempt),
		MetadataOptions: &types.InstanceMetadataOptionsRequest{HttpTokens: types.HttpTokensStateRequired},
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeInstance, Tags: tags},
			{ResourceType: types.ResourceTypeVolume, Tags: tags},
		},
	}
	if c.SubnetID != "" {
		in.SubnetId = sdk.String(c.SubnetID)
	}
	if len(c.SecurityGroupIDs) > 0 {
		in.SecurityGroupIds = c.SecurityGroupIDs
	}
	out, err := cl.RunInstances(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("aws: %w", err)
	}
	if len(out.Instances) != 1 {
		return nil, fmt.Errorf("aws: RunInstances returned %d instances", len(out.Instances))
	}
	s, _ := server(out.Instances[0], o.ServiceID)
	s.Attempt = o.Attempt
	return &s, nil
}

func (c *Client) Destroy(ctx context.Context, id string) error {
	cl, err := c.ec2()
	if err != nil {
		return err
	}
	_, err = cl.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}})
	var ae smithy.APIError
	if errors.As(err, &ae) && ae.ErrorCode() == "InvalidInstanceID.NotFound" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("aws: %w", err)
	}
	return nil
}

// Ping is a read-only call that proves the credentials and region work.
func (c *Client) Ping(ctx context.Context) error {
	cl, err := c.ec2()
	if err != nil {
		return err
	}
	if _, err := cl.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{}); err != nil {
		return fmt.Errorf("aws: %w", err)
	}
	return nil
}
