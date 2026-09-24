package awsclient

import (
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestParseRoutesKeepsPrivateNetworkTargets(t *testing.T) {
	routes := []ec2types.Route{
		{DestinationCidrBlock: aws.String("10.0.0.0/16"), GatewayId: aws.String("local"), State: ec2types.RouteStateActive},
		{DestinationCidrBlock: aws.String("10.20.0.0/16"), VpcPeeringConnectionId: aws.String("pcx-123"), State: ec2types.RouteStateActive},
		{DestinationCidrBlock: aws.String("10.30.0.0/16"), TransitGatewayId: aws.String("tgw-123"), State: ec2types.RouteStateActive},
		{DestinationCidrBlock: aws.String("0.0.0.0/0"), TransitGatewayId: aws.String("tgw-123"), State: ec2types.RouteStateActive},
		{DestinationCidrBlock: aws.String("10.40.0.0/16"), TransitGatewayId: aws.String("tgw-dead"), State: ec2types.RouteStateBlackhole},
	}
	want := []Route{
		{CIDR: "10.20.0.0/16", TargetType: "vpc_peering", TargetID: "pcx-123"},
		{CIDR: "10.30.0.0/16", TargetType: "transit_gateway", TargetID: "tgw-123"},
	}
	if got := parseRoutes(routes); !reflect.DeepEqual(got, want) {
		t.Fatalf("parseRoutes() = %#v, want %#v", got, want)
	}
}
