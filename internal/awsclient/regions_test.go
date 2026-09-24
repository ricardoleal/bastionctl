package awsclient

import (
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestRegionNamesSortsDeduplicatesAndSkipsEmpty(t *testing.T) {
	regions := []ec2types.Region{
		{RegionName: aws.String("us-east-1")},
		{RegionName: aws.String("eu-central-1")},
		{RegionName: aws.String("us-east-1")},
		{},
	}
	want := []string{"eu-central-1", "us-east-1"}
	if got := regionNames(regions); !reflect.DeepEqual(got, want) {
		t.Fatalf("regionNames() = %v, want %v", got, want)
	}
}
