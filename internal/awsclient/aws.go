// Package awsclient wraps the read-only AWS SDK calls needed to discover
// VPCs, instances, subnet routes, SSM status, and security group egress.
package awsclient

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Client wraps the EC2/SSM/STS clients built from the default credential
// chain (works transparently with whatever the TVM session exports).
type Client struct {
	ec2 *ec2.Client
	iam *iam.Client
	ssm *ssm.Client
	sts *sts.Client
	cfg aws.Config
}

// Identity is the result of sts:GetCallerIdentity.
type Identity struct {
	AccountID string
	Arn       string
}

// VPC is a discovered VPC and all of its associated CIDR blocks
// (primary + secondary, IPv4 + IPv6).
type VPC struct {
	VpcID      string
	Name       string
	CIDRBlocks []string
}

// Instance is a running EC2 instance relevant to pivot selection.
type Instance struct {
	InstanceID       string
	NameTag          string
	State            string
	PublicIP         string
	PrivateIP        string
	SubnetID         string
	SecurityGroupIDs []string
}

type Route struct {
	CIDR       string
	TargetType string
	TargetID   string
}

// New builds a Client using the default AWS credential/config chain,
// optionally overriding the region.
func New(ctx context.Context, regionOverride string, profile ...string) (*Client, error) {
	var opts []func(*awscfg.LoadOptions) error
	if regionOverride != "" {
		opts = append(opts, awscfg.WithRegion(regionOverride))
	}
	if len(profile) > 0 && profile[0] != "" {
		opts = append(opts, awscfg.WithSharedConfigProfile(profile[0]))
	}
	cfg, err := awscfg.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("no AWS region configured; pass --region or configure a default region")
	}
	return &Client{
		ec2: ec2.NewFromConfig(cfg),
		iam: iam.NewFromConfig(cfg),
		ssm: ssm.NewFromConfig(cfg),
		sts: sts.NewFromConfig(cfg),
		cfg: cfg,
	}, nil
}

// Region returns the resolved region for this client.
func (c *Client) Region() string { return c.cfg.Region }

// GetAccountAlias returns the account's IAM alias when one is configured.
// Callers should treat access errors as non-fatal because aliases are optional.
func (c *Client) GetAccountAlias(ctx context.Context) (string, error) {
	out, err := c.iam.ListAccountAliases(ctx, &iam.ListAccountAliasesInput{})
	if err != nil {
		return "", fmt.Errorf("iam:ListAccountAliases: %w", err)
	}
	if len(out.AccountAliases) == 0 {
		return "", nil
	}
	return out.AccountAliases[0], nil
}

// ListEnabledRegions returns regions enabled for the active account, sorted by
// region name. DescribeRegions is global but is issued through the current EC2
// client because the AWS SDK still requires a configured endpoint region.
func (c *Client) ListEnabledRegions(ctx context.Context) ([]string, error) {
	out, err := c.ec2.DescribeRegions(ctx, &ec2.DescribeRegionsInput{AllRegions: aws.Bool(false)})
	if err != nil {
		return nil, fmt.Errorf("describing enabled regions: %w", err)
	}
	return regionNames(out.Regions), nil
}

func regionNames(regions []ec2types.Region) []string {
	seen := make(map[string]struct{}, len(regions))
	names := make([]string, 0, len(regions))
	for _, region := range regions {
		name := aws.ToString(region.RegionName)
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// GetCallerIdentity calls sts:GetCallerIdentity.
func (c *Client) GetCallerIdentity(ctx context.Context) (*Identity, error) {
	out, err := c.sts.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, fmt.Errorf("sts:GetCallerIdentity: %w", err)
	}
	return &Identity{AccountID: aws.ToString(out.Account), Arn: aws.ToString(out.Arn)}, nil
}

// ListTargetVPCs resolves which VPC(s) to scan.
// mode is one of: "explicit" (vpcID was given), "auto_single" (only one
// VPC exists), "auto_all" (multiple VPCs, caller should scan/group all).
func (c *Client) ListTargetVPCs(ctx context.Context, vpcID string) (vpcs []VPC, mode string, err error) {
	if vpcID != "" {
		out, err := c.ec2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{VpcIds: []string{vpcID}})
		if err != nil {
			return nil, "", fmt.Errorf("describing vpc %s: %w", vpcID, err)
		}
		if len(out.Vpcs) == 0 {
			return nil, "", fmt.Errorf("VPC %s not found in this account/region", vpcID)
		}
		return []VPC{toVPC(out.Vpcs[0])}, "explicit", nil
	}

	paginator := ec2.NewDescribeVpcsPaginator(c.ec2, &ec2.DescribeVpcsInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("describing vpcs: %w", err)
		}
		for _, v := range page.Vpcs {
			vpcs = append(vpcs, toVPC(v))
		}
	}
	if len(vpcs) == 0 {
		return nil, "", fmt.Errorf("no VPCs found in this account/region")
	}
	if len(vpcs) == 1 {
		return vpcs, "auto_single", nil
	}
	return vpcs, "auto_all", nil
}

func toVPC(v ec2types.Vpc) VPC {
	var cidrs []string
	for _, assoc := range v.CidrBlockAssociationSet {
		if assoc.CidrBlockState != nil && assoc.CidrBlockState.State == ec2types.VpcCidrBlockStateCodeAssociated {
			cidrs = append(cidrs, aws.ToString(assoc.CidrBlock))
		}
	}
	for _, assoc := range v.Ipv6CidrBlockAssociationSet {
		if assoc.Ipv6CidrBlockState != nil && assoc.Ipv6CidrBlockState.State == ec2types.VpcCidrBlockStateCodeAssociated {
			cidrs = append(cidrs, aws.ToString(assoc.Ipv6CidrBlock))
		}
	}
	return VPC{VpcID: aws.ToString(v.VpcId), Name: nameTag(v.Tags), CIDRBlocks: cidrs}
}

// ListRunningInstances returns running instances in the given VPC.
func (c *Client) ListRunningInstances(ctx context.Context, vpcID string) ([]Instance, error) {
	var instances []Instance
	paginator := ec2.NewDescribeInstancesPaginator(c.ec2, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
			{Name: aws.String("instance-state-name"), Values: []string{"running"}},
		},
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("describing instances: %w", err)
		}
		for _, res := range page.Reservations {
			for _, inst := range res.Instances {
				instances = append(instances, Instance{
					InstanceID:       aws.ToString(inst.InstanceId),
					NameTag:          nameTag(inst.Tags),
					State:            string(inst.State.Name),
					PublicIP:         aws.ToString(inst.PublicIpAddress),
					PrivateIP:        aws.ToString(inst.PrivateIpAddress),
					SubnetID:         aws.ToString(inst.SubnetId),
					SecurityGroupIDs: instanceSecurityGroupIDs(inst),
				})
			}
		}
	}
	return instances, nil
}

func (c *Client) GetEffectiveSubnetRoutes(ctx context.Context, vpcID, subnetID string) ([]Route, error) {
	out, err := c.ec2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		Filters: []ec2types.Filter{{Name: aws.String("association.subnet-id"), Values: []string{subnetID}}},
	})
	if err != nil {
		return nil, fmt.Errorf("describing route table for subnet %s: %w", subnetID, err)
	}
	if len(out.RouteTables) == 0 {
		out, err = c.ec2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
			Filters: []ec2types.Filter{
				{Name: aws.String("vpc-id"), Values: []string{vpcID}},
				{Name: aws.String("association.main"), Values: []string{"true"}},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("describing main route table for VPC %s: %w", vpcID, err)
		}
	}
	if len(out.RouteTables) != 1 {
		return nil, fmt.Errorf("expected one effective route table for subnet %s, found %d", subnetID, len(out.RouteTables))
	}
	return parseRoutes(out.RouteTables[0].Routes), nil
}

func parseRoutes(routes []ec2types.Route) []Route {
	var result []Route
	for _, route := range routes {
		if route.State != ec2types.RouteStateActive {
			continue
		}
		cidr := aws.ToString(route.DestinationCidrBlock)
		if cidr == "" {
			cidr = aws.ToString(route.DestinationIpv6CidrBlock)
		}
		if cidr == "" || cidr == "0.0.0.0/0" || cidr == "::/0" {
			continue
		}

		targetType, targetID := routeTarget(route)
		if targetType == "" {
			continue
		}
		result = append(result, Route{CIDR: cidr, TargetType: targetType, TargetID: targetID})
	}
	return result
}

func routeTarget(route ec2types.Route) (string, string) {
	switch {
	case route.VpcPeeringConnectionId != nil:
		return "vpc_peering", aws.ToString(route.VpcPeeringConnectionId)
	case route.TransitGatewayId != nil:
		return "transit_gateway", aws.ToString(route.TransitGatewayId)
	case route.CoreNetworkArn != nil:
		return "core_network", aws.ToString(route.CoreNetworkArn)
	case route.LocalGatewayId != nil:
		return "local_gateway", aws.ToString(route.LocalGatewayId)
	case route.NetworkInterfaceId != nil:
		return "network_interface", aws.ToString(route.NetworkInterfaceId)
	case route.InstanceId != nil:
		return "instance", aws.ToString(route.InstanceId)
	case route.GatewayId != nil && strings.HasPrefix(aws.ToString(route.GatewayId), "vgw-"):
		return "vpn_gateway", aws.ToString(route.GatewayId)
	default:
		return "", ""
	}
}

// instanceSecurityGroupIDs collects the union of Security Group IDs across
// an instance's primary SecurityGroups field and all of its ENIs (an
// instance can have multiple network interfaces, each with its own SG
// set). Deduplicated.
func instanceSecurityGroupIDs(inst ec2types.Instance) []string {
	seen := map[string]struct{}{}
	var ids []string
	add := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	for _, sg := range inst.SecurityGroups {
		add(aws.ToString(sg.GroupId))
	}
	for _, eni := range inst.NetworkInterfaces {
		for _, sg := range eni.Groups {
			add(aws.ToString(sg.GroupId))
		}
	}
	return ids
}

// EgressRule is a simplified view of one destination CIDR permitted by a
// Security Group's egress rules (SG-to-SG references and managed prefix
// lists are not resolved here -- see README limitations).
type EgressRule struct {
	CIDR string
}

// DescribeSecurityGroupEgress returns, for each requested SG ID, the list
// of destination CIDRs its egress rules allow.
func (c *Client) DescribeSecurityGroupEgress(ctx context.Context, sgIDs []string) (map[string][]EgressRule, error) {
	result := make(map[string][]EgressRule)
	if len(sgIDs) == 0 {
		return result, nil
	}

	const chunkSize = 200
	for i := 0; i < len(sgIDs); i += chunkSize {
		end := i + chunkSize
		if end > len(sgIDs) {
			end = len(sgIDs)
		}
		chunk := sgIDs[i:end]

		paginator := ec2.NewDescribeSecurityGroupsPaginator(c.ec2, &ec2.DescribeSecurityGroupsInput{GroupIds: chunk})
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("describing security groups: %w", err)
			}
			for _, sg := range page.SecurityGroups {
				id := aws.ToString(sg.GroupId)
				var rules []EgressRule
				for _, perm := range sg.IpPermissionsEgress {
					for _, r := range perm.IpRanges {
						rules = append(rules, EgressRule{CIDR: aws.ToString(r.CidrIp)})
					}
					for _, r := range perm.Ipv6Ranges {
						rules = append(rules, EgressRule{CIDR: aws.ToString(r.CidrIpv6)})
					}
					// Note: UserIdGroupPairs (SG-to-SG) and PrefixListIds
					// sources are intentionally not resolved here; a
					// candidate relying solely on those for VPC-wide
					// egress will show as a coverage gap.
				}
				result[id] = rules
			}
		}
	}
	return result, nil
}

// ListSSMOnlineInstanceIDs returns the subset of the given instance IDs
// that AWS Systems Manager currently reports as PingStatus=Online -- i.e.
// reachable via `aws ssm start-session` / Session Manager, independent of
// any VPC network path, public IP, or Security Group rule. This is the
// primary reachability signal in environments (like this one) that use
// SSM-brokered SSH instead of direct network access.
func (c *Client) ListSSMOnlineInstanceIDs(ctx context.Context, instanceIDs []string) (map[string]bool, error) {
	online := make(map[string]bool)
	if len(instanceIDs) == 0 {
		return online, nil
	}

	const chunkSize = 50 // defensive batch size for the InstanceIds filter
	for i := 0; i < len(instanceIDs); i += chunkSize {
		end := i + chunkSize
		if end > len(instanceIDs) {
			end = len(instanceIDs)
		}
		chunk := instanceIDs[i:end]

		var nextToken *string
		for {
			out, err := c.ssm.DescribeInstanceInformation(ctx, &ssm.DescribeInstanceInformationInput{
				Filters: []ssmtypes.InstanceInformationStringFilter{
					{Key: aws.String("InstanceIds"), Values: chunk},
				},
				NextToken: nextToken,
			})
			if err != nil {
				return nil, fmt.Errorf("ssm:DescribeInstanceInformation: %w", err)
			}
			for _, info := range out.InstanceInformationList {
				if info.PingStatus == ssmtypes.PingStatusOnline {
					online[aws.ToString(info.InstanceId)] = true
				}
			}
			if out.NextToken == nil || aws.ToString(out.NextToken) == "" {
				break
			}
			nextToken = out.NextToken
		}
	}
	return online, nil
}

func nameTag(tags []ec2types.Tag) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == "Name" {
			return aws.ToString(t.Value)
		}
	}
	return ""
}
