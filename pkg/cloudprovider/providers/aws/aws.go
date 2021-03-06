/*
Copyright 2014 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package aws

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elb"
	"github.com/scalingdata/gcfg"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/cloudprovider"
	"k8s.io/kubernetes/pkg/credentialprovider/aws"
	"k8s.io/kubernetes/pkg/types"
	"k8s.io/kubernetes/pkg/util/sets"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api/unversioned"
)

const ProviderName = "aws"

// The tag name we use to differentiate multiple logically independent clusters running in the same AZ
const TagNameKubernetesCluster = "KubernetesCluster"

// The tag name we use to differentiate multiple services. Used currently for ELBs only.
const TagNameKubernetesService = "kubernetes.io/service-name"

// We sometimes read to see if something exists; then try to create it if we didn't find it
// This can fail once in a consistent system if done in parallel
// In an eventually consistent system, it could fail unboundedly
// MaxReadThenCreateRetries sets the maximum number of attempts we will make
const MaxReadThenCreateRetries = 30

// Default volume type for newly created Volumes
// TODO: Remove when user/admin can configure volume types and thus we don't
// need hardcoded defaults.
const DefaultVolumeType = "gp2"

// Used to call aws_credentials.Init() just once
var once sync.Once

// Abstraction over AWS, to allow mocking/other implementations
type AWSServices interface {
	Compute(region string) (EC2, error)
	LoadBalancing(region string) (ELB, error)
	Autoscaling(region string) (ASG, error)
	Metadata() (EC2Metadata, error)
}

// TODO: Should we rename this to AWS (EBS & ELB are not technically part of EC2)
// Abstraction over EC2, to allow mocking/other implementations
// Note that the DescribeX functions return a list, so callers don't need to deal with paging
type EC2 interface {
	// Query EC2 for instances matching the filter
	DescribeInstances(request *ec2.DescribeInstancesInput) ([]*ec2.Instance, error)

	// Attach a volume to an instance
	AttachVolume(*ec2.AttachVolumeInput) (*ec2.VolumeAttachment, error)
	// Detach a volume from an instance it is attached to
	DetachVolume(request *ec2.DetachVolumeInput) (resp *ec2.VolumeAttachment, err error)
	// Lists volumes
	DescribeVolumes(request *ec2.DescribeVolumesInput) ([]*ec2.Volume, error)
	// Create an EBS volume
	CreateVolume(request *ec2.CreateVolumeInput) (resp *ec2.Volume, err error)
	// Delete an EBS volume
	DeleteVolume(*ec2.DeleteVolumeInput) (*ec2.DeleteVolumeOutput, error)

	DescribeSecurityGroups(request *ec2.DescribeSecurityGroupsInput) ([]*ec2.SecurityGroup, error)

	CreateSecurityGroup(*ec2.CreateSecurityGroupInput) (*ec2.CreateSecurityGroupOutput, error)
	DeleteSecurityGroup(request *ec2.DeleteSecurityGroupInput) (*ec2.DeleteSecurityGroupOutput, error)

	AuthorizeSecurityGroupIngress(*ec2.AuthorizeSecurityGroupIngressInput) (*ec2.AuthorizeSecurityGroupIngressOutput, error)
	RevokeSecurityGroupIngress(*ec2.RevokeSecurityGroupIngressInput) (*ec2.RevokeSecurityGroupIngressOutput, error)

	DescribeSubnets(*ec2.DescribeSubnetsInput) ([]*ec2.Subnet, error)

	CreateTags(*ec2.CreateTagsInput) (*ec2.CreateTagsOutput, error)

	DescribeRouteTables(request *ec2.DescribeRouteTablesInput) ([]*ec2.RouteTable, error)
	CreateRoute(request *ec2.CreateRouteInput) (*ec2.CreateRouteOutput, error)
	DeleteRoute(request *ec2.DeleteRouteInput) (*ec2.DeleteRouteOutput, error)

	ModifyInstanceAttribute(request *ec2.ModifyInstanceAttributeInput) (*ec2.ModifyInstanceAttributeOutput, error)
}

// This is a simple pass-through of the ELB client interface, which allows for testing
type ELB interface {
	CreateLoadBalancer(*elb.CreateLoadBalancerInput) (*elb.CreateLoadBalancerOutput, error)
	DeleteLoadBalancer(*elb.DeleteLoadBalancerInput) (*elb.DeleteLoadBalancerOutput, error)
	DescribeLoadBalancers(*elb.DescribeLoadBalancersInput) (*elb.DescribeLoadBalancersOutput, error)
	RegisterInstancesWithLoadBalancer(*elb.RegisterInstancesWithLoadBalancerInput) (*elb.RegisterInstancesWithLoadBalancerOutput, error)
	DeregisterInstancesFromLoadBalancer(*elb.DeregisterInstancesFromLoadBalancerInput) (*elb.DeregisterInstancesFromLoadBalancerOutput, error)

	DetachLoadBalancerFromSubnets(*elb.DetachLoadBalancerFromSubnetsInput) (*elb.DetachLoadBalancerFromSubnetsOutput, error)
	AttachLoadBalancerToSubnets(*elb.AttachLoadBalancerToSubnetsInput) (*elb.AttachLoadBalancerToSubnetsOutput, error)

	CreateLoadBalancerListeners(*elb.CreateLoadBalancerListenersInput) (*elb.CreateLoadBalancerListenersOutput, error)
	DeleteLoadBalancerListeners(*elb.DeleteLoadBalancerListenersInput) (*elb.DeleteLoadBalancerListenersOutput, error)

	ApplySecurityGroupsToLoadBalancer(*elb.ApplySecurityGroupsToLoadBalancerInput) (*elb.ApplySecurityGroupsToLoadBalancerOutput, error)

	ConfigureHealthCheck(*elb.ConfigureHealthCheckInput) (*elb.ConfigureHealthCheckOutput, error)
}

// This is a simple pass-through of the Autoscaling client interface, which allows for testing
type ASG interface {
	UpdateAutoScalingGroup(*autoscaling.UpdateAutoScalingGroupInput) (*autoscaling.UpdateAutoScalingGroupOutput, error)
	DescribeAutoScalingGroups(*autoscaling.DescribeAutoScalingGroupsInput) (*autoscaling.DescribeAutoScalingGroupsOutput, error)
}

// Abstraction over the AWS metadata service
type EC2Metadata interface {
	// Query the EC2 metadata service (used to discover instance-id etc)
	GetMetadata(path string) (string, error)
}

type VolumeOptions struct {
	CapacityGB int
	Tags       *map[string]string
}

// Volumes is an interface for managing cloud-provisioned volumes
// TODO: Allow other clouds to implement this
type Volumes interface {
	// Attach the disk to the specified instance
	// instanceName can be empty to mean "the instance on which we are running"
	// Returns the device (e.g. /dev/xvdf) where we attached the volume
	AttachDisk(diskName string, instanceName string, readOnly bool) (string, error)
	// Detach the disk from the specified instance
	// instanceName can be empty to mean "the instance on which we are running"
	// Returns the device where the volume was attached
	DetachDisk(diskName string, instanceName string) (string, error)

	// Create a volume with the specified options
	CreateDisk(volumeOptions *VolumeOptions) (volumeName string, err error)
	// Delete the specified volume
	// Returns true iff the volume was deleted
	// If the was not found, returns (false, nil)
	DeleteDisk(volumeName string) (bool, error)

	// Get labels to apply to volume on creation
	GetVolumeLabels(volumeName string) (map[string]string, error)
}

// InstanceGroups is an interface for managing cloud-managed instance groups / autoscaling instance groups
// TODO: Allow other clouds to implement this
type InstanceGroups interface {
	// Set the size to the fixed size
	ResizeInstanceGroup(instanceGroupName string, size int) error
	// Queries the cloud provider for information about the specified instance group
	DescribeInstanceGroup(instanceGroupName string) (InstanceGroupInfo, error)
}

// InstanceGroupInfo is returned by InstanceGroups.Describe, and exposes information about the group.
type InstanceGroupInfo interface {
	// The number of instances currently running under control of this group
	CurrentSize() (int, error)
}

// AWSCloud is an implementation of Interface, LoadBalancer and Instances for Amazon Web Services.
type AWSCloud struct {
	ec2              EC2
	elb              ELB
	asg              ASG
	metadata         EC2Metadata
	cfg              *AWSCloudConfig
	availabilityZone string
	region           string

	filterTags map[string]string

	// The AWS instance that we are running on
	selfAWSInstance *awsInstance

	mutex sync.Mutex
}

var _ Volumes = &AWSCloud{}

type AWSCloudConfig struct {
	Global struct {
		// TODO: Is there any use for this?  We can get it from the instance metadata service
		// Maybe if we're not running on AWS, e.g. bootstrap; for now it is not very useful
		Zone string

		KubernetesClusterTag string
	}
}

// awsSdkEC2 is an implementation of the EC2 interface, backed by aws-sdk-go
type awsSdkEC2 struct {
	ec2 *ec2.EC2
}

type awsSDKProvider struct {
	creds *credentials.Credentials

	mutex          sync.Mutex
	regionDelayers map[string]*CrossRequestRetryDelay
}

func newAWSSDKProvider(creds *credentials.Credentials) *awsSDKProvider {
	return &awsSDKProvider{
		creds:          creds,
		regionDelayers: make(map[string]*CrossRequestRetryDelay),
	}
}

func (p *awsSDKProvider) addHandlers(regionName string, h *request.Handlers) {
	h.Sign.PushFrontNamed(request.NamedHandler{
		Name: "k8s/logger",
		Fn:   awsHandlerLogger,
	})

	delayer := p.getCrossRequestRetryDelay(regionName)
	if delayer != nil {
		h.Sign.PushFrontNamed(request.NamedHandler{
			Name: "k8s/delay-presign",
			Fn:   delayer.BeforeSign,
		})

		h.AfterRetry.PushFrontNamed(request.NamedHandler{
			Name: "k8s/delay-afterretry",
			Fn:   delayer.AfterRetry,
		})
	}
}

// Get a CrossRequestRetryDelay, scoped to the region, not to the request.
// This means that when we hit a limit on a call, we will delay _all_ calls to the API.
// We do this to protect the AWS account from becoming overloaded and effectively locked.
// We also log when we hit request limits.
// Note that this delays the current goroutine; this is bad behaviour and will
// likely cause k8s to become slow or unresponsive for cloud operations.
// However, this throttle is intended only as a last resort.  When we observe
// this throttling, we need to address the root cause (e.g. add a delay to a
// controller retry loop)
func (p *awsSDKProvider) getCrossRequestRetryDelay(regionName string) *CrossRequestRetryDelay {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	delayer, found := p.regionDelayers[regionName]
	if !found {
		delayer = NewCrossRequestRetryDelay()
		p.regionDelayers[regionName] = delayer
	}
	return delayer
}

func (p *awsSDKProvider) Compute(regionName string) (EC2, error) {
	service := ec2.New(session.New(&aws.Config{
		Region:      &regionName,
		Credentials: p.creds,
	}))

	p.addHandlers(regionName, &service.Handlers)

	ec2 := &awsSdkEC2{
		ec2: service,
	}
	return ec2, nil
}

func (p *awsSDKProvider) LoadBalancing(regionName string) (ELB, error) {
	elbClient := elb.New(session.New(&aws.Config{
		Region:      &regionName,
		Credentials: p.creds,
	}))

	p.addHandlers(regionName, &elbClient.Handlers)

	return elbClient, nil
}

func (p *awsSDKProvider) Autoscaling(regionName string) (ASG, error) {
	client := autoscaling.New(session.New(&aws.Config{
		Region:      &regionName,
		Credentials: p.creds,
	}))

	p.addHandlers(regionName, &client.Handlers)

	return client, nil
}

func (p *awsSDKProvider) Metadata() (EC2Metadata, error) {
	client := ec2metadata.New(session.New(&aws.Config{}))
	return client, nil
}

func stringPointerArray(orig []string) []*string {
	if orig == nil {
		return nil
	}
	n := make([]*string, len(orig))
	for i := range orig {
		n[i] = &orig[i]
	}
	return n
}

func isNilOrEmpty(s *string) bool {
	return s == nil || *s == ""
}

func orEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func newEc2Filter(name string, value string) *ec2.Filter {
	filter := &ec2.Filter{
		Name: aws.String(name),
		Values: []*string{
			aws.String(value),
		},
	}
	return filter
}

func (self *AWSCloud) AddSSHKeyToAllInstances(user string, keyData []byte) error {
	return errors.New("unimplemented")
}

func (a *AWSCloud) CurrentNodeName(hostname string) (string, error) {
	selfInstance, err := a.getSelfAWSInstance()
	if err != nil {
		return "", err
	}
	return selfInstance.nodeName, nil
}

// Implementation of EC2.Instances
func (self *awsSdkEC2) DescribeInstances(request *ec2.DescribeInstancesInput) ([]*ec2.Instance, error) {
	// Instances are paged
	results := []*ec2.Instance{}
	var nextToken *string

	for {
		response, err := self.ec2.DescribeInstances(request)
		if err != nil {
			return nil, fmt.Errorf("error listing AWS instances: %v", err)
		}

		for _, reservation := range response.Reservations {
			results = append(results, reservation.Instances...)
		}

		nextToken = response.NextToken
		if isNilOrEmpty(nextToken) {
			break
		}
		request.NextToken = nextToken
	}

	return results, nil
}

// Implements EC2.DescribeSecurityGroups
func (s *awsSdkEC2) DescribeSecurityGroups(request *ec2.DescribeSecurityGroupsInput) ([]*ec2.SecurityGroup, error) {
	// Security groups are not paged
	response, err := s.ec2.DescribeSecurityGroups(request)
	if err != nil {
		return nil, fmt.Errorf("error listing AWS security groups: %v", err)
	}
	return response.SecurityGroups, nil
}

func (s *awsSdkEC2) AttachVolume(request *ec2.AttachVolumeInput) (*ec2.VolumeAttachment, error) {
	return s.ec2.AttachVolume(request)
}

func (s *awsSdkEC2) DetachVolume(request *ec2.DetachVolumeInput) (*ec2.VolumeAttachment, error) {
	return s.ec2.DetachVolume(request)
}

func (s *awsSdkEC2) DescribeVolumes(request *ec2.DescribeVolumesInput) ([]*ec2.Volume, error) {
	// Volumes are paged
	results := []*ec2.Volume{}
	var nextToken *string

	for {
		response, err := s.ec2.DescribeVolumes(request)

		if err != nil {
			return nil, fmt.Errorf("error listing AWS volumes: %v", err)
		}

		results = append(results, response.Volumes...)

		nextToken = response.NextToken
		if isNilOrEmpty(nextToken) {
			break
		}
		request.NextToken = nextToken
	}

	return results, nil
}

func (s *awsSdkEC2) CreateVolume(request *ec2.CreateVolumeInput) (resp *ec2.Volume, err error) {
	return s.ec2.CreateVolume(request)
}

func (s *awsSdkEC2) DeleteVolume(request *ec2.DeleteVolumeInput) (*ec2.DeleteVolumeOutput, error) {
	return s.ec2.DeleteVolume(request)
}

func (s *awsSdkEC2) DescribeSubnets(request *ec2.DescribeSubnetsInput) ([]*ec2.Subnet, error) {
	// Subnets are not paged
	response, err := s.ec2.DescribeSubnets(request)
	if err != nil {
		return nil, fmt.Errorf("error listing AWS subnets: %v", err)
	}
	return response.Subnets, nil
}

func (s *awsSdkEC2) CreateSecurityGroup(request *ec2.CreateSecurityGroupInput) (*ec2.CreateSecurityGroupOutput, error) {
	return s.ec2.CreateSecurityGroup(request)
}

func (s *awsSdkEC2) DeleteSecurityGroup(request *ec2.DeleteSecurityGroupInput) (*ec2.DeleteSecurityGroupOutput, error) {
	return s.ec2.DeleteSecurityGroup(request)
}

func (s *awsSdkEC2) AuthorizeSecurityGroupIngress(request *ec2.AuthorizeSecurityGroupIngressInput) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	return s.ec2.AuthorizeSecurityGroupIngress(request)
}

func (s *awsSdkEC2) RevokeSecurityGroupIngress(request *ec2.RevokeSecurityGroupIngressInput) (*ec2.RevokeSecurityGroupIngressOutput, error) {
	return s.ec2.RevokeSecurityGroupIngress(request)
}

func (s *awsSdkEC2) CreateTags(request *ec2.CreateTagsInput) (*ec2.CreateTagsOutput, error) {
	return s.ec2.CreateTags(request)
}

func (s *awsSdkEC2) DescribeRouteTables(request *ec2.DescribeRouteTablesInput) ([]*ec2.RouteTable, error) {
	// Not paged
	response, err := s.ec2.DescribeRouteTables(request)
	if err != nil {
		return nil, fmt.Errorf("error listing AWS route tables: %v", err)
	}
	return response.RouteTables, nil
}

func (s *awsSdkEC2) CreateRoute(request *ec2.CreateRouteInput) (*ec2.CreateRouteOutput, error) {
	return s.ec2.CreateRoute(request)
}

func (s *awsSdkEC2) DeleteRoute(request *ec2.DeleteRouteInput) (*ec2.DeleteRouteOutput, error) {
	return s.ec2.DeleteRoute(request)
}

func (s *awsSdkEC2) ModifyInstanceAttribute(request *ec2.ModifyInstanceAttributeInput) (*ec2.ModifyInstanceAttributeOutput, error) {
	return s.ec2.ModifyInstanceAttribute(request)
}

func init() {
	cloudprovider.RegisterCloudProvider(ProviderName, func(config io.Reader) (cloudprovider.Interface, error) {
		creds := credentials.NewChainCredentials(
			[]credentials.Provider{
				&credentials.EnvProvider{},
				&ec2rolecreds.EC2RoleProvider{
					Client: ec2metadata.New(session.New(&aws.Config{})),
				},
				&credentials.SharedCredentialsProvider{},
			})
		aws := newAWSSDKProvider(creds)
		return newAWSCloud(config, aws)
	})
}

// readAWSCloudConfig reads an instance of AWSCloudConfig from config reader.
func readAWSCloudConfig(config io.Reader, metadata EC2Metadata) (*AWSCloudConfig, error) {
	var cfg AWSCloudConfig
	var err error

	if config != nil {
		err = gcfg.ReadInto(&cfg, config)
		if err != nil {
			return nil, err
		}
	}

	if cfg.Global.Zone == "" {
		if metadata != nil {
			glog.Info("Zone not specified in configuration file; querying AWS metadata service")
			cfg.Global.Zone, err = getAvailabilityZone(metadata)
			if err != nil {
				return nil, err
			}
		}
		if cfg.Global.Zone == "" {
			return nil, fmt.Errorf("no zone specified in configuration file")
		}
	}

	return &cfg, nil
}

func getInstanceType(metadata EC2Metadata) (string, error) {
	return metadata.GetMetadata("instance-type")
}

func getAvailabilityZone(metadata EC2Metadata) (string, error) {
	return metadata.GetMetadata("placement/availability-zone")
}

func isRegionValid(region string) bool {
	regions := [...]string{
		"us-east-1",
		"us-west-1",
		"us-west-2",
		"eu-west-1",
		"eu-central-1",
		"ap-southeast-1",
		"ap-southeast-2",
		"ap-northeast-1",
		"cn-north-1",
		"us-gov-west-1",
		"sa-east-1",
	}
	for _, r := range regions {
		if r == region {
			return true
		}
	}
	return false
}

// Derives the region from a valid az name.
// Returns an error if the az is known invalid (empty)
func azToRegion(az string) (string, error) {
	if len(az) < 1 {
		return "", fmt.Errorf("invalid (empty) AZ")
	}
	region := az[:len(az)-1]
	return region, nil
}

// newAWSCloud creates a new instance of AWSCloud.
// AWSProvider and instanceId are primarily for tests
func newAWSCloud(config io.Reader, awsServices AWSServices) (*AWSCloud, error) {
	metadata, err := awsServices.Metadata()
	if err != nil {
		return nil, fmt.Errorf("error creating AWS metadata client: %v", err)
	}

	cfg, err := readAWSCloudConfig(config, metadata)
	if err != nil {
		return nil, fmt.Errorf("unable to read AWS cloud provider config file: %v", err)
	}

	zone := cfg.Global.Zone
	if len(zone) <= 1 {
		return nil, fmt.Errorf("invalid AWS zone in config file: %s", zone)
	}
	regionName, err := azToRegion(zone)
	if err != nil {
		return nil, err
	}

	valid := isRegionValid(regionName)
	if !valid {
		return nil, fmt.Errorf("not a valid AWS zone (unknown region): %s", zone)
	}

	ec2, err := awsServices.Compute(regionName)
	if err != nil {
		return nil, fmt.Errorf("error creating AWS EC2 client: %v", err)
	}

	elb, err := awsServices.LoadBalancing(regionName)
	if err != nil {
		return nil, fmt.Errorf("error creating AWS ELB client: %v", err)
	}

	asg, err := awsServices.Autoscaling(regionName)
	if err != nil {
		return nil, fmt.Errorf("error creating AWS autoscaling client: %v", err)
	}

	awsCloud := &AWSCloud{
		ec2:              ec2,
		elb:              elb,
		asg:              asg,
		metadata:         metadata,
		cfg:              cfg,
		region:           regionName,
		availabilityZone: zone,
	}

	filterTags := map[string]string{}
	if cfg.Global.KubernetesClusterTag != "" {
		filterTags[TagNameKubernetesCluster] = cfg.Global.KubernetesClusterTag
	} else {
		selfInstance, err := awsCloud.getSelfAWSInstance()
		if err != nil {
			return nil, err
		}
		selfInstanceInfo, err := selfInstance.getInfo()
		if err != nil {
			return nil, err
		}
		for _, tag := range selfInstanceInfo.Tags {
			if orEmpty(tag.Key) == TagNameKubernetesCluster {
				filterTags[TagNameKubernetesCluster] = orEmpty(tag.Value)
			}
		}
	}

	awsCloud.filterTags = filterTags
	if len(filterTags) > 0 {
		glog.Infof("AWS cloud filtering on tags: %v", filterTags)
	} else {
		glog.Infof("AWS cloud - no tag filtering")
	}

	// Register handler for ECR credentials
	once.Do(func() {
		aws_credentials.Init()
	})

	return awsCloud, nil
}

func (aws *AWSCloud) Clusters() (cloudprovider.Clusters, bool) {
	return nil, false
}

// ProviderName returns the cloud provider ID.
func (aws *AWSCloud) ProviderName() string {
	return ProviderName
}

// ScrubDNS filters DNS settings for pods.
func (aws *AWSCloud) ScrubDNS(nameservers, searches []string) (nsOut, srchOut []string) {
	return nameservers, searches
}

// LoadBalancer returns an implementation of LoadBalancer for Amazon Web Services.
func (s *AWSCloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	return s, true
}

// Instances returns an implementation of Instances for Amazon Web Services.
func (aws *AWSCloud) Instances() (cloudprovider.Instances, bool) {
	return aws, true
}

// Zones returns an implementation of Zones for Amazon Web Services.
func (aws *AWSCloud) Zones() (cloudprovider.Zones, bool) {
	return aws, true
}

// Routes returns an implementation of Routes for Amazon Web Services.
func (aws *AWSCloud) Routes() (cloudprovider.Routes, bool) {
	return aws, true
}

// NodeAddresses is an implementation of Instances.NodeAddresses.
func (aws *AWSCloud) NodeAddresses(name string) ([]api.NodeAddress, error) {
	self, err := aws.getSelfAWSInstance()
	if err != nil {
		return nil, err
	}
	if self.nodeName == name || len(name) == 0 {
		addresses := []api.NodeAddress{}

		internalIP, err := aws.metadata.GetMetadata("local-ipv4")
		if err != nil {
			return nil, err
		}
		addresses = append(addresses, api.NodeAddress{Type: api.NodeInternalIP, Address: internalIP})
		// Legacy compatibility: the private ip was the legacy host ip
		addresses = append(addresses, api.NodeAddress{Type: api.NodeLegacyHostIP, Address: internalIP})

		externalIP, err := aws.metadata.GetMetadata("public-ipv4")
		if err != nil {
			//TODO: It would be nice to be able to determine the reason for the failure,
			// but the AWS client masks all failures with the same error description.
			glog.V(2).Info("Could not determine public IP from AWS metadata.")
		} else {
			addresses = append(addresses, api.NodeAddress{Type: api.NodeExternalIP, Address: externalIP})
		}

		return addresses, nil
	}
	instance, err := aws.getInstanceByNodeName(name)
	if err != nil {
		return nil, err
	}

	addresses := []api.NodeAddress{}

	if !isNilOrEmpty(instance.PrivateIpAddress) {
		ipAddress := *instance.PrivateIpAddress
		ip := net.ParseIP(ipAddress)
		if ip == nil {
			return nil, fmt.Errorf("EC2 instance had invalid private address: %s (%s)", orEmpty(instance.InstanceId), ipAddress)
		}
		addresses = append(addresses, api.NodeAddress{Type: api.NodeInternalIP, Address: ip.String()})

		// Legacy compatibility: the private ip was the legacy host ip
		addresses = append(addresses, api.NodeAddress{Type: api.NodeLegacyHostIP, Address: ip.String()})
	}

	// TODO: Other IP addresses (multiple ips)?
	if !isNilOrEmpty(instance.PublicIpAddress) {
		ipAddress := *instance.PublicIpAddress
		ip := net.ParseIP(ipAddress)
		if ip == nil {
			return nil, fmt.Errorf("EC2 instance had invalid public address: %s (%s)", orEmpty(instance.InstanceId), ipAddress)
		}
		addresses = append(addresses, api.NodeAddress{Type: api.NodeExternalIP, Address: ip.String()})
	}

	return addresses, nil
}

// ExternalID returns the cloud provider ID of the specified instance (deprecated).
func (aws *AWSCloud) ExternalID(name string) (string, error) {
	awsInstance, err := aws.getSelfAWSInstance()
	if err != nil {
		return "", err
	}

	if awsInstance.nodeName == name {
		// We assume that if this is run on the instance itself, the instance exists and is alive
		return awsInstance.awsID, nil
	} else {
		// We must verify that the instance still exists
		// Note that if the instance does not exist or is no longer running, we must return ("", cloudprovider.InstanceNotFound)
		instance, err := aws.findInstanceByNodeName(name)
		if err != nil {
			return "", err
		}
		if instance == nil || !isAlive(instance) {
			return "", cloudprovider.InstanceNotFound
		}
		return orEmpty(instance.InstanceId), nil
	}
}

// InstanceID returns the cloud provider ID of the specified instance.
func (aws *AWSCloud) InstanceID(name string) (string, error) {
	awsInstance, err := aws.getSelfAWSInstance()
	if err != nil {
		return "", err
	}

	// In the future it is possible to also return an endpoint as:
	// <endpoint>/<zone>/<instanceid>
	if awsInstance.nodeName == name {
		return "/" + awsInstance.availabilityZone + "/" + awsInstance.awsID, nil
	} else {
		inst, err := aws.getInstanceByNodeName(name)
		if err != nil {
			return "", err
		}
		return "/" + orEmpty(inst.Placement.AvailabilityZone) + "/" + orEmpty(inst.InstanceId), nil
	}
}

// InstanceType returns the type of the specified instance.
func (aws *AWSCloud) InstanceType(name string) (string, error) {
	awsInstance, err := aws.getSelfAWSInstance()
	if err != nil {
		return "", err
	}

	if awsInstance.nodeName == name {
		return awsInstance.instanceType, nil
	} else {
		inst, err := aws.getInstanceByNodeName(name)
		if err != nil {
			return "", err
		}
		return orEmpty(inst.InstanceType), nil
	}
}

// Check if the instance is alive (running or pending)
// We typically ignore instances that are not alive
func isAlive(instance *ec2.Instance) bool {
	if instance.State == nil {
		glog.Warning("Instance state was unexpectedly nil: ", instance)
		return false
	}
	stateName := orEmpty(instance.State.Name)
	switch stateName {
	case "shutting-down", "terminated", "stopping", "stopped":
		return false
	case "pending", "running":
		return true
	default:
		glog.Errorf("Unknown EC2 instance state: %s", stateName)
		return false
	}
}

// Return a list of instances matching regex string.
func (s *AWSCloud) getInstancesByRegex(regex string) ([]string, error) {
	filters := []*ec2.Filter{}
	filters = s.addFilters(filters)
	request := &ec2.DescribeInstancesInput{
		Filters: filters,
	}

	instances, err := s.ec2.DescribeInstances(request)
	if err != nil {
		return []string{}, err
	}
	if len(instances) == 0 {
		return []string{}, fmt.Errorf("no instances returned")
	}

	if strings.HasPrefix(regex, "'") && strings.HasSuffix(regex, "'") {
		glog.Infof("Stripping quotes around regex (%s)", regex)
		regex = regex[1 : len(regex)-1]
	}

	re, err := regexp.Compile(regex)
	if err != nil {
		return []string{}, err
	}

	matchingInstances := []string{}
	for _, instance := range instances {
		// TODO: Push filtering down into EC2 API filter?
		if !isAlive(instance) {
			continue
		}

		// Only return fully-ready instances when listing instances
		// (vs a query by name, where we will return it if we find it)
		if orEmpty(instance.State.Name) == "pending" {
			glog.V(2).Infof("Skipping EC2 instance (pending): %s", *instance.InstanceId)
			continue
		}

		privateDNSName := orEmpty(instance.PrivateDnsName)
		if privateDNSName == "" {
			glog.V(2).Infof("Skipping EC2 instance (no PrivateDNSName): %s",
				orEmpty(instance.InstanceId))
			continue
		}

		for _, tag := range instance.Tags {
			if orEmpty(tag.Key) == "Name" && re.MatchString(orEmpty(tag.Value)) {
				matchingInstances = append(matchingInstances, privateDNSName)
				break
			}
		}
	}
	glog.V(2).Infof("Matched EC2 instances: %s", matchingInstances)
	return matchingInstances, nil
}

// List is an implementation of Instances.List.
func (aws *AWSCloud) List(filter string) ([]string, error) {
	// TODO: Should really use tag query. No need to go regexp.
	return aws.getInstancesByRegex(filter)
}

// GetZone implements Zones.GetZone
func (self *AWSCloud) GetZone() (cloudprovider.Zone, error) {
	return cloudprovider.Zone{
		FailureDomain: self.availabilityZone,
		Region:        self.region,
	}, nil
}

// Abstraction around AWS Instance Types
// There isn't an API to get information for a particular instance type (that I know of)
type awsInstanceType struct {
}

// Used to represent a mount device for attaching an EBS volume
// This should be stored as a single letter (i.e. c, not sdc or /dev/sdc)
type mountDevice string

// TODO: Also return number of mounts allowed?
func (self *awsInstanceType) getEBSMountDevices() []mountDevice {
	// See: https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/block-device-mapping-concepts.html
	devices := []mountDevice{}
	for c := 'f'; c <= 'p'; c++ {
		devices = append(devices, mountDevice(fmt.Sprintf("%c", c)))
	}
	return devices
}

type awsInstance struct {
	ec2 EC2

	// id in AWS
	awsID string

	// node name in k8s
	nodeName string

	// availability zone the instance resides in
	availabilityZone string

	// instance type
	instanceType string

	mutex sync.Mutex

	// We must cache because otherwise there is a race condition,
	// where we assign a device mapping and then get a second request before we attach the volume
	deviceMappings map[mountDevice]string
}

func newAWSInstance(ec2 EC2, awsID, nodeName, availabilityZone, instanceType string) *awsInstance {
	self := &awsInstance{ec2: ec2, awsID: awsID, nodeName: nodeName, availabilityZone: availabilityZone, instanceType: instanceType}

	// We lazy-init deviceMappings
	self.deviceMappings = nil

	return self
}

// Gets the awsInstanceType that models the instance type of this instance
func (self *awsInstance) getInstanceType() *awsInstanceType {
	// TODO: Make this real
	awsInstanceType := &awsInstanceType{}
	return awsInstanceType
}

// Gets the full information about this instance from the EC2 API
func (self *awsInstance) getInfo() (*ec2.Instance, error) {
	instanceID := self.awsID
	request := &ec2.DescribeInstancesInput{
		InstanceIds: []*string{&instanceID},
	}

	instances, err := self.ec2.DescribeInstances(request)
	if err != nil {
		return nil, err
	}
	if len(instances) == 0 {
		return nil, fmt.Errorf("no instances found for instance: %s", self.awsID)
	}
	if len(instances) > 1 {
		return nil, fmt.Errorf("multiple instances found for instance: %s", self.awsID)
	}
	return instances[0], nil
}

// Gets the mountDevice already assigned to the volume, or assigns an unused mountDevice.
// If the volume is already assigned, this will return the existing mountDevice with alreadyAttached=true.
// Otherwise the mountDevice is assigned by finding the first available mountDevice, and it is returned with alreadyAttached=false.
func (self *awsInstance) getMountDevice(volumeID string, assign bool) (assigned mountDevice, alreadyAttached bool, err error) {
	instanceType := self.getInstanceType()
	if instanceType == nil {
		return "", false, fmt.Errorf("could not get instance type for instance: %s", self.awsID)
	}

	// We lock to prevent concurrent mounts from conflicting
	// We may still conflict if someone calls the API concurrently,
	// but the AWS API will then fail one of the two attach operations
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// We cache both for efficiency and correctness
	if self.deviceMappings == nil {
		info, err := self.getInfo()
		if err != nil {
			return "", false, err
		}
		deviceMappings := map[mountDevice]string{}
		for _, blockDevice := range info.BlockDeviceMappings {
			name := aws.StringValue(blockDevice.DeviceName)
			if strings.HasPrefix(name, "/dev/sd") {
				name = name[7:]
			}
			if strings.HasPrefix(name, "/dev/xvd") {
				name = name[8:]
			}
			if len(name) != 1 {
				glog.Warningf("Unexpected EBS DeviceName: %q", aws.StringValue(blockDevice.DeviceName))
			}
			deviceMappings[mountDevice(name)] = aws.StringValue(blockDevice.Ebs.VolumeId)
		}
		self.deviceMappings = deviceMappings
	}

	// Check to see if this volume is already assigned a device on this machine
	for mountDevice, mappingVolumeID := range self.deviceMappings {
		if volumeID == mappingVolumeID {
			if assign {
				glog.Warningf("Got assignment call for already-assigned volume: %s@%s", mountDevice, mappingVolumeID)
			}
			return mountDevice, true, nil
		}
	}

	if !assign {
		return mountDevice(""), false, nil
	}

	// Check all the valid mountpoints to see if any of them are free
	valid := instanceType.getEBSMountDevices()
	chosen := mountDevice("")
	for _, mountDevice := range valid {
		_, found := self.deviceMappings[mountDevice]
		if !found {
			chosen = mountDevice
			break
		}
	}

	if chosen == "" {
		glog.Warningf("Could not assign a mount device (all in use?).  mappings=%v, valid=%v", self.deviceMappings, valid)
		return "", false, nil
	}

	self.deviceMappings[chosen] = volumeID
	glog.V(2).Infof("Assigned mount device %s -> volume %s", chosen, volumeID)

	return chosen, false, nil
}

func (self *awsInstance) releaseMountDevice(volumeID string, mountDevice mountDevice) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	existingVolumeID, found := self.deviceMappings[mountDevice]
	if !found {
		glog.Errorf("releaseMountDevice on non-allocated device")
		return
	}
	if volumeID != existingVolumeID {
		glog.Errorf("releaseMountDevice on device assigned to different volume")
		return
	}
	glog.V(2).Infof("Releasing mount device mapping: %s -> volume %s", mountDevice, volumeID)
	delete(self.deviceMappings, mountDevice)
}

type awsDisk struct {
	ec2 EC2

	// Name in k8s
	name string
	// id in AWS
	awsID string
	// az which holds the volume
	az string
}

func newAWSDisk(aws *AWSCloud, name string) (*awsDisk, error) {
	if !strings.HasPrefix(name, "aws://") {
		name = "aws://" + aws.availabilityZone + "/" + name
	}
	// name looks like aws://availability-zone/id
	url, err := url.Parse(name)
	if err != nil {
		// TODO: Maybe we should pass a URL into the Volume functions
		return nil, fmt.Errorf("Invalid disk name (%s): %v", name, err)
	}
	if url.Scheme != "aws" {
		return nil, fmt.Errorf("Invalid scheme for AWS volume (%s)", name)
	}

	awsID := url.Path
	if len(awsID) > 1 && awsID[0] == '/' {
		awsID = awsID[1:]
	}

	// TODO: Regex match?
	if strings.Contains(awsID, "/") || !strings.HasPrefix(awsID, "vol-") {
		return nil, fmt.Errorf("Invalid format for AWS volume (%s)", name)
	}
	az := url.Host
	// TODO: Better validation?
	// TODO: Default to our AZ?  Look it up?
	// TODO: Should this be a region or an AZ?
	if az == "" {
		return nil, fmt.Errorf("Invalid format for AWS volume (%s)", name)
	}
	disk := &awsDisk{ec2: aws.ec2, name: name, awsID: awsID, az: az}
	return disk, nil
}

// Gets the full information about this volume from the EC2 API
func (self *awsDisk) getInfo() (*ec2.Volume, error) {
	volumeID := self.awsID

	request := &ec2.DescribeVolumesInput{
		VolumeIds: []*string{&volumeID},
	}

	volumes, err := self.ec2.DescribeVolumes(request)
	if err != nil {
		return nil, fmt.Errorf("error querying ec2 for volume info: %v", err)
	}
	if len(volumes) == 0 {
		return nil, fmt.Errorf("no volumes found for volume: %s", self.awsID)
	}
	if len(volumes) > 1 {
		return nil, fmt.Errorf("multiple volumes found for volume: %s", self.awsID)
	}
	return volumes[0], nil
}

func (self *awsDisk) waitForAttachmentStatus(status string) error {
	// TODO: There may be a faster way to get this when we're attaching locally
	attempt := 0
	maxAttempts := 60

	for {
		info, err := self.getInfo()
		if err != nil {
			return err
		}
		if len(info.Attachments) > 1 {
			glog.Warningf("Found multiple attachments for volume: %v", info)
		}
		attachmentStatus := ""
		for _, attachment := range info.Attachments {
			if attachmentStatus != "" {
				glog.Warning("Found multiple attachments: ", info)
			}
			if attachment.State != nil {
				attachmentStatus = *attachment.State
			} else {
				// Shouldn't happen, but don't panic...
				glog.Warning("Ignoring nil attachment state: ", attachment)
			}
		}
		if attachmentStatus == "" {
			attachmentStatus = "detached"
		}
		if attachmentStatus == status {
			return nil
		}

		glog.V(2).Infof("Waiting for volume state: actual=%s, desired=%s", attachmentStatus, status)

		attempt++
		if attempt > maxAttempts {
			glog.Warningf("Timeout waiting for volume state: actual=%s, desired=%s", attachmentStatus, status)
			return errors.New("Timeout waiting for volume state")
		}

		time.Sleep(1 * time.Second)
	}
}

// Deletes the EBS disk
func (self *awsDisk) deleteVolume() (bool, error) {
	request := &ec2.DeleteVolumeInput{VolumeId: aws.String(self.awsID)}
	_, err := self.ec2.DeleteVolume(request)
	if err != nil {
		if awsError, ok := err.(awserr.Error); ok {
			if awsError.Code() == "InvalidVolume.NotFound" {
				return false, nil
			}
		}
		return false, fmt.Errorf("error deleting EBS volumes: %v", err)
	}
	return true, nil
}

// Gets the awsInstance for the EC2 instance on which we are running
// may return nil in case of error
func (s *AWSCloud) getSelfAWSInstance() (*awsInstance, error) {
	// Note that we cache some state in awsInstance (mountpoints), so we must preserve the instance

	s.mutex.Lock()
	defer s.mutex.Unlock()

	i := s.selfAWSInstance
	if i == nil {
		instanceId, err := s.metadata.GetMetadata("instance-id")
		if err != nil {
			return nil, fmt.Errorf("error fetching instance-id from ec2 metadata service: %v", err)
		}
		// privateDnsName, err := s.metadata.GetMetadata("local-hostname")
		// See #11543 - need to use ec2 API to get the privateDnsName in case of private dns zone e.g. mydomain.io
		instance, err := s.getInstanceByID(instanceId)
		if err != nil {
			return nil, fmt.Errorf("error finding instance %s: %v", instanceId, err)
		}
		privateDnsName := aws.StringValue(instance.PrivateDnsName)
		availabilityZone, err := getAvailabilityZone(s.metadata)
		if err != nil {
			return nil, fmt.Errorf("error fetching availability zone from ec2 metadata service: %v", err)
		}
		instanceType, err := getInstanceType(s.metadata)
		if err != nil {
			return nil, fmt.Errorf("error fetching instance type from ec2 metadata service: %v", err)
		}

		i = newAWSInstance(s.ec2, instanceId, privateDnsName, availabilityZone, instanceType)
		s.selfAWSInstance = i
	}

	return i, nil
}

// Gets the awsInstance with node-name nodeName, or the 'self' instance if nodeName == ""
func (aws *AWSCloud) getAwsInstance(nodeName string) (*awsInstance, error) {
	var awsInstance *awsInstance
	var err error
	if nodeName == "" {
		awsInstance, err = aws.getSelfAWSInstance()
		if err != nil {
			return nil, fmt.Errorf("error getting self-instance: %v", err)
		}
	} else {
		instance, err := aws.getInstanceByNodeName(nodeName)
		if err != nil {
			return nil, fmt.Errorf("error finding instance %s: %v", nodeName, err)
		}

		awsInstance = newAWSInstance(aws.ec2, orEmpty(instance.InstanceId), orEmpty(instance.PrivateDnsName), orEmpty(instance.Placement.AvailabilityZone), orEmpty(instance.InstanceType))
	}

	return awsInstance, nil
}

// Implements Volumes.AttachDisk
func (c *AWSCloud) AttachDisk(diskName string, instanceName string, readOnly bool) (string, error) {
	disk, err := newAWSDisk(c, diskName)
	if err != nil {
		return "", err
	}

	awsInstance, err := c.getAwsInstance(instanceName)
	if err != nil {
		return "", err
	}

	if readOnly {
		// TODO: We could enforce this when we mount the volume (?)
		// TODO: We could also snapshot the volume and attach copies of it
		return "", errors.New("AWS volumes cannot be mounted read-only")
	}

	mountDevice, alreadyAttached, err := awsInstance.getMountDevice(disk.awsID, true)
	if err != nil {
		return "", err
	}

	// Inside the instance, the mountpoint always looks like /dev/xvdX (?)
	hostDevice := "/dev/xvd" + string(mountDevice)
	// In the EC2 API, it is sometimes is /dev/sdX and sometimes /dev/xvdX
	// We are running on the node here, so we check if /dev/xvda exists to determine this
	ec2Device := "/dev/xvd" + string(mountDevice)
	if _, err := os.Stat("/dev/xvda"); os.IsNotExist(err) {
		ec2Device = "/dev/sd" + string(mountDevice)
	}

	attached := false
	defer func() {
		if !attached {
			awsInstance.releaseMountDevice(disk.awsID, mountDevice)
		}
	}()

	if !alreadyAttached {
		request := &ec2.AttachVolumeInput{
			Device:     aws.String(ec2Device),
			InstanceId: aws.String(awsInstance.awsID),
			VolumeId:   aws.String(disk.awsID),
		}

		attachResponse, err := c.ec2.AttachVolume(request)
		if err != nil {
			// TODO: Check if the volume was concurrently attached?
			return "", fmt.Errorf("Error attaching EBS volume: %v", err)
		}

		glog.V(2).Infof("AttachVolume request returned %v", attachResponse)
	}

	err = disk.waitForAttachmentStatus("attached")
	if err != nil {
		return "", err
	}

	attached = true

	return hostDevice, nil
}

// Implements Volumes.DetachDisk
func (aws *AWSCloud) DetachDisk(diskName string, instanceName string) (string, error) {
	disk, err := newAWSDisk(aws, diskName)
	if err != nil {
		return "", err
	}

	awsInstance, err := aws.getAwsInstance(instanceName)
	if err != nil {
		return "", err
	}

	mountDevice, alreadyAttached, err := awsInstance.getMountDevice(disk.awsID, false)
	if err != nil {
		return "", err
	}

	if !alreadyAttached {
		glog.Warning("DetachDisk called on non-attached disk: ", diskName)
		// TODO: Continue?  Tolerate non-attached error in DetachVolume?
	}

	request := ec2.DetachVolumeInput{
		InstanceId: &awsInstance.awsID,
		VolumeId:   &disk.awsID,
	}

	response, err := aws.ec2.DetachVolume(&request)
	if err != nil {
		return "", fmt.Errorf("error detaching EBS volume: %v", err)
	}
	if response == nil {
		return "", errors.New("no response from DetachVolume")
	}

	// TODO: Fix this - just remove the cache?
	// If we don't have a cache; we don't have to wait any more (the driver does it for us)
	// Also, maybe we could get the locally connected drivers from the AWS metadata service?

	// At this point we are waiting for the volume being detached. This
	// releases the volume and invalidates the cache even when there is a timeout.
	//
	// TODO: A timeout leaves the cache in an inconsistent state. The volume is still
	// detaching though the cache shows it as ready to be attached again. Subsequent
	// attach operations will fail. The attach is being retried and eventually
	// works though. An option would be to completely flush the cache upon timeouts.
	//
	defer func() {
		// TODO: Not thread safe?
		for mountDevice, existingVolumeID := range awsInstance.deviceMappings {
			if existingVolumeID == disk.awsID {
				awsInstance.releaseMountDevice(disk.awsID, mountDevice)
				return
			}
		}
	}()

	err = disk.waitForAttachmentStatus("detached")
	if err != nil {
		return "", err
	}

	hostDevicePath := "/dev/xvd" + string(mountDevice)
	return hostDevicePath, err
}

// Implements Volumes.CreateVolume
func (s *AWSCloud) CreateDisk(volumeOptions *VolumeOptions) (string, error) {
	// TODO: Should we tag this with the cluster id (so it gets deleted when the cluster does?)

	request := &ec2.CreateVolumeInput{}
	request.AvailabilityZone = &s.availabilityZone
	volSize := int64(volumeOptions.CapacityGB)
	request.Size = &volSize
	request.VolumeType = aws.String(DefaultVolumeType)
	response, err := s.ec2.CreateVolume(request)
	if err != nil {
		return "", err
	}

	az := orEmpty(response.AvailabilityZone)
	awsID := orEmpty(response.VolumeId)

	volumeName := "aws://" + az + "/" + awsID

	// apply tags
	if volumeOptions.Tags != nil {
		tags := []*ec2.Tag{}
		for k, v := range *volumeOptions.Tags {
			tag := &ec2.Tag{}
			tag.Key = aws.String(k)
			tag.Value = aws.String(v)
			tags = append(tags, tag)
		}
		tagRequest := &ec2.CreateTagsInput{}
		tagRequest.Resources = []*string{&awsID}
		tagRequest.Tags = tags
		if _, err := s.createTags(tagRequest); err != nil {
			// delete the volume and hope it succeeds
			_, delerr := s.DeleteDisk(volumeName)
			if delerr != nil {
				// delete did not succeed, we have a stray volume!
				return "", fmt.Errorf("error tagging volume %s, could not delete the volume: %v", volumeName, delerr)
			}
			return "", fmt.Errorf("error tagging volume %s: %v", volumeName, err)
		}
	}
	return volumeName, nil
}

// Implements Volumes.DeleteDisk
func (aws *AWSCloud) DeleteDisk(volumeName string) (bool, error) {
	awsDisk, err := newAWSDisk(aws, volumeName)
	if err != nil {
		return false, err
	}
	return awsDisk.deleteVolume()
}

// Implements Volumes.GetVolumeLabels
func (c *AWSCloud) GetVolumeLabels(volumeName string) (map[string]string, error) {
	awsDisk, err := newAWSDisk(c, volumeName)
	if err != nil {
		return nil, err
	}
	info, err := awsDisk.getInfo()
	if err != nil {
		return nil, err
	}
	labels := make(map[string]string)
	az := aws.StringValue(info.AvailabilityZone)
	if az == "" {
		return nil, fmt.Errorf("volume did not have AZ information: %q", info.VolumeId)
	}

	labels[unversioned.LabelZoneFailureDomain] = az
	region, err := azToRegion(az)
	if err != nil {
		return nil, err
	}
	labels[unversioned.LabelZoneRegion] = region

	return labels, nil
}

// Gets the current load balancer state
func (s *AWSCloud) describeLoadBalancer(name string) (*elb.LoadBalancerDescription, error) {
	request := &elb.DescribeLoadBalancersInput{}
	request.LoadBalancerNames = []*string{&name}

	response, err := s.elb.DescribeLoadBalancers(request)
	if err != nil {
		if awsError, ok := err.(awserr.Error); ok {
			if awsError.Code() == "LoadBalancerNotFound" {
				return nil, nil
			}
		}
		return nil, err
	}

	var ret *elb.LoadBalancerDescription
	for _, loadBalancer := range response.LoadBalancerDescriptions {
		if ret != nil {
			glog.Errorf("Found multiple load balancers with name: %s", name)
		}
		ret = loadBalancer
	}
	return ret, nil
}

// Retrieves instance's vpc id from metadata
func (self *AWSCloud) findVPCID() (string, error) {
	macs, err := self.metadata.GetMetadata("network/interfaces/macs/")
	if err != nil {
		return "", fmt.Errorf("Could not list interfaces of the instance", err)
	}

	// loop over interfaces, first vpc id returned wins
	for _, macPath := range strings.Split(macs, "\n") {
		if len(macPath) == 0 {
			continue
		}
		url := fmt.Sprintf("network/interfaces/macs/%svpc-id", macPath)
		vpcID, err := self.metadata.GetMetadata(url)
		if err != nil {
			continue
		}
		return vpcID, nil
	}
	return "", fmt.Errorf("Could not find VPC ID in instance metadata")
}

// Retrieves the specified security group from the AWS API, or returns nil if not found
func (s *AWSCloud) findSecurityGroup(securityGroupId string) (*ec2.SecurityGroup, error) {
	describeSecurityGroupsRequest := &ec2.DescribeSecurityGroupsInput{
		GroupIds: []*string{&securityGroupId},
	}

	groups, err := s.ec2.DescribeSecurityGroups(describeSecurityGroupsRequest)
	if err != nil {
		glog.Warning("Error retrieving security group", err)
		return nil, err
	}

	if len(groups) == 0 {
		return nil, nil
	}
	if len(groups) != 1 {
		// This should not be possible - ids should be unique
		return nil, fmt.Errorf("multiple security groups found with same id")
	}
	group := groups[0]
	return group, nil
}

func isEqualIntPointer(l, r *int64) bool {
	if l == nil {
		return r == nil
	}
	if r == nil {
		return l == nil
	}
	return *l == *r
}

func isEqualStringPointer(l, r *string) bool {
	if l == nil {
		return r == nil
	}
	if r == nil {
		return l == nil
	}
	return *l == *r
}

func ipPermissionExists(newPermission, existing *ec2.IpPermission, compareGroupUserIDs bool) bool {
	if !isEqualIntPointer(newPermission.FromPort, existing.FromPort) {
		return false
	}
	if !isEqualIntPointer(newPermission.ToPort, existing.ToPort) {
		return false
	}
	if !isEqualStringPointer(newPermission.IpProtocol, existing.IpProtocol) {
		return false
	}
	// Check only if newPermission is a subset of existing. Usually it has zero or one elements.
	// Not doing actual CIDR math yet; not clear it's needed, either.
	glog.V(4).Infof("Comparing %v to %v", newPermission, existing)
	if len(newPermission.IpRanges) > len(existing.IpRanges) {
		return false
	}

	for j := range newPermission.IpRanges {
		found := false
		for k := range existing.IpRanges {
			if isEqualStringPointer(newPermission.IpRanges[j].CidrIp, existing.IpRanges[k].CidrIp) {
				found = true
				break
			}
		}
		if found == false {
			return false
		}
	}
	for _, leftPair := range newPermission.UserIdGroupPairs {
		for _, rightPair := range existing.UserIdGroupPairs {
			if isEqualUserGroupPair(leftPair, rightPair, compareGroupUserIDs) {
				return true
			}
		}
		return false
	}

	return true
}

func isEqualUserGroupPair(l, r *ec2.UserIdGroupPair, compareGroupUserIDs bool) bool {
	glog.V(2).Infof("Comparing %v to %v", *l.GroupId, *r.GroupId)
	if isEqualStringPointer(l.GroupId, r.GroupId) {
		if compareGroupUserIDs {
			if isEqualStringPointer(l.UserId, r.UserId) {
				return true
			}
		} else {
			return true
		}
	}

	return false
}

// Makes sure the security group includes the specified permissions
// Returns true if and only if changes were made
// The security group must already exist
func (s *AWSCloud) ensureSecurityGroupIngress(securityGroupId string, addPermissions []*ec2.IpPermission) (bool, error) {
	group, err := s.findSecurityGroup(securityGroupId)
	if err != nil {
		glog.Warning("Error retrieving security group", err)
		return false, err
	}

	if group == nil {
		return false, fmt.Errorf("security group not found: %s", securityGroupId)
	}

	glog.V(2).Infof("Existing security group ingress: %s %v", securityGroupId, group.IpPermissions)

	changes := []*ec2.IpPermission{}
	for _, addPermission := range addPermissions {
		hasUserID := false
		for i := range addPermission.UserIdGroupPairs {
			if addPermission.UserIdGroupPairs[i].UserId != nil {
				hasUserID = true
			}
		}

		found := false
		for _, groupPermission := range group.IpPermissions {
			if ipPermissionExists(addPermission, groupPermission, hasUserID) {
				found = true
				break
			}
		}

		if !found {
			changes = append(changes, addPermission)
		}
	}

	if len(changes) == 0 {
		return false, nil
	}

	glog.V(2).Infof("Adding security group ingress: %s %v", securityGroupId, changes)

	request := &ec2.AuthorizeSecurityGroupIngressInput{}
	request.GroupId = &securityGroupId
	request.IpPermissions = changes
	_, err = s.ec2.AuthorizeSecurityGroupIngress(request)
	if err != nil {
		glog.Warning("Error authorizing security group ingress", err)
		return false, err
	}

	return true, nil
}

// Makes sure the security group no longer includes the specified permissions
// Returns true if and only if changes were made
// If the security group no longer exists, will return (false, nil)
func (s *AWSCloud) removeSecurityGroupIngress(securityGroupId string, removePermissions []*ec2.IpPermission) (bool, error) {
	group, err := s.findSecurityGroup(securityGroupId)
	if err != nil {
		glog.Warning("Error retrieving security group", err)
		return false, err
	}

	if group == nil {
		glog.Warning("Security group not found: ", securityGroupId)
		return false, nil
	}

	changes := []*ec2.IpPermission{}
	for _, removePermission := range removePermissions {
		hasUserID := false
		for i := range removePermission.UserIdGroupPairs {
			if removePermission.UserIdGroupPairs[i].UserId != nil {
				hasUserID = true
			}
		}

		var found *ec2.IpPermission
		for _, groupPermission := range group.IpPermissions {
			if ipPermissionExists(removePermission, groupPermission, hasUserID) {
				found = removePermission
				break
			}
		}

		if found != nil {
			changes = append(changes, found)
		}
	}

	if len(changes) == 0 {
		return false, nil
	}

	glog.V(2).Infof("Removing security group ingress: %s %v", securityGroupId, changes)

	request := &ec2.RevokeSecurityGroupIngressInput{}
	request.GroupId = &securityGroupId
	request.IpPermissions = changes
	_, err = s.ec2.RevokeSecurityGroupIngress(request)
	if err != nil {
		glog.Warning("Error revoking security group ingress", err)
		return false, err
	}

	return true, nil
}

// Makes sure the security group exists
// Returns the security group id or error
func (s *AWSCloud) ensureSecurityGroup(name string, description string, vpcID string) (string, error) {
	groupID := ""
	attempt := 0
	for {
		attempt++

		request := &ec2.DescribeSecurityGroupsInput{}
		filters := []*ec2.Filter{
			newEc2Filter("group-name", name),
			newEc2Filter("vpc-id", vpcID),
		}
		request.Filters = s.addFilters(filters)

		securityGroups, err := s.ec2.DescribeSecurityGroups(request)
		if err != nil {
			return "", err
		}

		if len(securityGroups) >= 1 {
			if len(securityGroups) > 1 {
				glog.Warning("Found multiple security groups with name:", name)
			}
			return orEmpty(securityGroups[0].GroupId), nil
		}

		createRequest := &ec2.CreateSecurityGroupInput{}
		createRequest.VpcId = &vpcID
		createRequest.GroupName = &name
		createRequest.Description = &description

		createResponse, err := s.ec2.CreateSecurityGroup(createRequest)
		if err != nil {
			ignore := false
			switch err := err.(type) {
			case awserr.Error:
				if err.Code() == "InvalidGroup.Duplicate" && attempt < MaxReadThenCreateRetries {
					glog.V(2).Infof("Got InvalidGroup.Duplicate while creating security group (race?); will retry")
					ignore = true
				}
			}
			if !ignore {
				glog.Error("Error creating security group: ", err)
				return "", err
			}
			time.Sleep(1 * time.Second)
		} else {
			groupID = orEmpty(createResponse.GroupId)
			break
		}
	}
	if groupID == "" {
		return "", fmt.Errorf("created security group, but id was not returned: %s", name)
	}

	tags := []*ec2.Tag{}
	for k, v := range s.filterTags {
		tag := &ec2.Tag{}
		tag.Key = aws.String(k)
		tag.Value = aws.String(v)
		tags = append(tags, tag)
	}

	if len(tags) > 0 {
		tagRequest := &ec2.CreateTagsInput{}
		tagRequest.Resources = []*string{&groupID}
		tagRequest.Tags = tags
		if _, err := s.createTags(tagRequest); err != nil {
			// Not clear how to recover fully from this; we're OK because we don't match on tags, but that is a little odd
			return "", fmt.Errorf("error tagging security group: %v", err)
		}
	}
	return groupID, nil
}

// createTags calls EC2 CreateTags, but adds retry-on-failure logic
// We retry mainly because if we create an object, we cannot tag it until it is "fully created" (eventual consistency)
// The error code varies though (depending on what we are tagging), so we simply retry on all errors
func (s *AWSCloud) createTags(request *ec2.CreateTagsInput) (*ec2.CreateTagsOutput, error) {
	// TODO: We really should do exponential backoff here
	attempt := 0
	maxAttempts := 60

	for {
		response, err := s.ec2.CreateTags(request)
		if err == nil {
			return response, err
		}

		// We could check that the error is retryable, but the error code changes based on what we are tagging
		// SecurityGroup: InvalidGroup.NotFound
		attempt++
		if attempt > maxAttempts {
			glog.Warningf("Failed to create tags (too many attempts): %v", err)
			return response, err
		}
		glog.V(2).Infof("Failed to create tags; will retry.  Error was %v", err)
		time.Sleep(1 * time.Second)
	}
}

func (s *AWSCloud) listSubnetIDsinVPC(vpcId string) ([]string, error) {

	subnetIds := []string{}

	request := &ec2.DescribeSubnetsInput{}
	filters := []*ec2.Filter{}
	filters = append(filters, newEc2Filter("vpc-id", vpcId))
	// Note, this will only return subnets tagged with the cluster identifier for this Kubernetes cluster.
	// In the case where an AZ has public & private subnets per AWS best practices, the deployment should ensure
	// only the public subnet (where the ELB will go) is so tagged.
	filters = s.addFilters(filters)
	request.Filters = filters

	subnets, err := s.ec2.DescribeSubnets(request)
	if err != nil {
		glog.Error("Error describing subnets: ", err)
		return nil, err
	}

	availabilityZones := sets.NewString()
	for _, subnet := range subnets {
		az := orEmpty(subnet.AvailabilityZone)
		id := orEmpty(subnet.SubnetId)
		if availabilityZones.Has(az) {
			glog.Warning("Found multiple subnets per AZ '", az, "', ignoring subnet '", id, "'")
			continue
		}
		subnetIds = append(subnetIds, id)
		availabilityZones.Insert(az)
	}

	return subnetIds, nil
}

// EnsureLoadBalancer implements LoadBalancer.EnsureLoadBalancer
// TODO(justinsb) It is weird that these take a region.  I suspect it won't work cross-region anyway.
func (s *AWSCloud) EnsureLoadBalancer(name, region string, publicIP net.IP, ports []*api.ServicePort, hosts []string, serviceName types.NamespacedName, affinity api.ServiceAffinity, annotations cloudprovider.ServiceAnnotation) (*api.LoadBalancerStatus, error) {
	glog.V(2).Infof("EnsureLoadBalancer(%v, %v, %v, %v, %v, %v, %v)", name, region, publicIP, ports, hosts, serviceName, annotations)

	if region != s.region {
		return nil, fmt.Errorf("requested load balancer region '%s' does not match cluster region '%s'", region, s.region)
	}

	if affinity != api.ServiceAffinityNone {
		// ELB supports sticky sessions, but only when configured for HTTP/HTTPS
		return nil, fmt.Errorf("unsupported load balancer affinity: %v", affinity)
	}

	if len(ports) == 0 {
		return nil, fmt.Errorf("requested load balancer with no ports")
	}

	for _, port := range ports {
		if port.Protocol != api.ProtocolTCP {
			return nil, fmt.Errorf("Only TCP LoadBalancer is supported for AWS ELB")
		}
	}

	if publicIP != nil {
		return nil, fmt.Errorf("publicIP cannot be specified for AWS ELB")
	}

	instances, err := s.getInstancesByNodeNames(hosts)
	if err != nil {
		return nil, err
	}

	vpcId, err := s.findVPCID()
	if err != nil {
		glog.Error("Error finding VPC", err)
		return nil, err
	}

	// Construct list of configured subnets
	subnetIDs, err := s.listSubnetIDsinVPC(vpcId)
	if err != nil {
		glog.Error("Error listing subnets in VPC", err)
		return nil, err
	}

	// Create a security group for the load balancer
	var securityGroupID string
	{
		sgName := "k8s-elb-" + name
		sgDescription := fmt.Sprintf("Security group for Kubernetes ELB %s (%v)", name, serviceName)
		securityGroupID, err = s.ensureSecurityGroup(sgName, sgDescription, vpcId)
		if err != nil {
			glog.Error("Error creating load balancer security group: ", err)
			return nil, err
		}

		permissions := []*ec2.IpPermission{}
		for _, port := range ports {
			portInt64 := int64(port.Port)
			protocol := strings.ToLower(string(port.Protocol))
			sourceIp := "0.0.0.0/0"

			permission := &ec2.IpPermission{}
			permission.FromPort = &portInt64
			permission.ToPort = &portInt64
			permission.IpRanges = []*ec2.IpRange{{CidrIp: &sourceIp}}
			permission.IpProtocol = &protocol

			permissions = append(permissions, permission)
		}
		_, err = s.ensureSecurityGroupIngress(securityGroupID, permissions)
		if err != nil {
			return nil, err
		}
	}
	securityGroupIDs := []string{securityGroupID}

	// Figure out what mappings we want on the load balancer
	listeners := []*elb.Listener{}
	for _, port := range ports {
		if port.NodePort == 0 {
			glog.Errorf("Ignoring port without NodePort defined: %v", port)
			continue
		}
		instancePort := int64(port.NodePort)
		loadBalancerPort := int64(port.Port)
		protocol := strings.ToLower(string(port.Protocol))

		listener := &elb.Listener{}
		listener.InstancePort = &instancePort
		listener.LoadBalancerPort = &loadBalancerPort
		listener.Protocol = &protocol
		listener.InstanceProtocol = &protocol

		listeners = append(listeners, listener)
	}

	// Build the load balancer itself
	loadBalancer, err := s.ensureLoadBalancer(serviceName, name, listeners, subnetIDs, securityGroupIDs)
	if err != nil {
		return nil, err
	}

	err = s.ensureLoadBalancerHealthCheck(loadBalancer, listeners)
	if err != nil {
		return nil, err
	}

	err = s.updateInstanceSecurityGroupsForLoadBalancer(loadBalancer, instances)
	if err != nil {
		glog.Warning("Error opening ingress rules for the load balancer to the instances: ", err)
		return nil, err
	}

	err = s.ensureLoadBalancerInstances(orEmpty(loadBalancer.LoadBalancerName), loadBalancer.Instances, instances)
	if err != nil {
		glog.Warningf("Error registering instances with the load balancer: %v", err)
		return nil, err
	}

	glog.V(1).Infof("Loadbalancer %s (%v) has DNS name %s", name, serviceName, orEmpty(loadBalancer.DNSName))

	// TODO: Wait for creation?

	status := toStatus(loadBalancer)
	return status, nil
}

// GetLoadBalancer is an implementation of LoadBalancer.GetLoadBalancer
func (s *AWSCloud) GetLoadBalancer(name, region string) (*api.LoadBalancerStatus, bool, error) {
	if region != s.region {
		return nil, false, fmt.Errorf("requested load balancer region '%s' does not match cluster region '%s'", region, s.region)
	}

	lb, err := s.describeLoadBalancer(name)
	if err != nil {
		return nil, false, err
	}

	if lb == nil {
		return nil, false, nil
	}

	status := toStatus(lb)
	return status, true, nil
}

func toStatus(lb *elb.LoadBalancerDescription) *api.LoadBalancerStatus {
	status := &api.LoadBalancerStatus{}

	if !isNilOrEmpty(lb.DNSName) {
		var ingress api.LoadBalancerIngress
		ingress.Hostname = orEmpty(lb.DNSName)
		status.Ingress = []api.LoadBalancerIngress{ingress}
	}

	return status
}

// Returns the first security group for an instance, or nil
// We only create instances with one security group, so we warn if there are multiple or none
func findSecurityGroupForInstance(instance *ec2.Instance) *string {
	var securityGroupId *string
	for _, securityGroup := range instance.SecurityGroups {
		if securityGroup == nil || securityGroup.GroupId == nil {
			// Not expected, but avoid panic
			glog.Warning("Unexpected empty security group for instance: ", orEmpty(instance.InstanceId))
			continue
		}

		if securityGroupId != nil {
			// We create instances with one SG
			glog.Warningf("Multiple security groups found for instance (%s); will use first group (%s)", orEmpty(instance.InstanceId), *securityGroupId)
			continue
		} else {
			securityGroupId = securityGroup.GroupId
		}
	}

	if securityGroupId == nil {
		glog.Warning("No security group found for instance ", orEmpty(instance.InstanceId))
	}

	return securityGroupId
}

// Open security group ingress rules on the instances so that the load balancer can talk to them
// Will also remove any security groups ingress rules for the load balancer that are _not_ needed for allInstances
func (s *AWSCloud) updateInstanceSecurityGroupsForLoadBalancer(lb *elb.LoadBalancerDescription, allInstances []*ec2.Instance) error {
	// Determine the load balancer security group id
	loadBalancerSecurityGroupId := ""
	for _, securityGroup := range lb.SecurityGroups {
		if isNilOrEmpty(securityGroup) {
			continue
		}
		if loadBalancerSecurityGroupId != "" {
			// We create LBs with one SG
			glog.Warning("Multiple security groups for load balancer: ", orEmpty(lb.LoadBalancerName))
		}
		loadBalancerSecurityGroupId = *securityGroup
	}
	if loadBalancerSecurityGroupId == "" {
		return fmt.Errorf("Could not determine security group for load balancer: %s", orEmpty(lb.LoadBalancerName))
	}

	// Get the actual list of groups that allow ingress from the load-balancer
	describeRequest := &ec2.DescribeSecurityGroupsInput{}
	filters := []*ec2.Filter{}
	filters = append(filters, newEc2Filter("ip-permission.group-id", loadBalancerSecurityGroupId))
	describeRequest.Filters = s.addFilters(filters)
	actualGroups, err := s.ec2.DescribeSecurityGroups(describeRequest)
	if err != nil {
		return fmt.Errorf("error querying security groups: %v", err)
	}

	// Open the firewall from the load balancer to the instance
	// We don't actually have a trivial way to know in advance which security group the instance is in
	// (it is probably the minion security group, but we don't easily have that).
	// However, we _do_ have the list of security groups on the instance records.

	// Map containing the changes we want to make; true to add, false to remove
	instanceSecurityGroupIds := map[string]bool{}

	// Scan instances for groups we want open
	for _, instance := range allInstances {
		securityGroupId := findSecurityGroupForInstance(instance)
		if isNilOrEmpty(securityGroupId) {
			glog.Warning("Ignoring instance without security group: ", orEmpty(instance.InstanceId))
			continue
		}

		instanceSecurityGroupIds[*securityGroupId] = true
	}

	// Compare to actual groups
	for _, actualGroup := range actualGroups {
		if isNilOrEmpty(actualGroup.GroupId) {
			glog.Warning("Ignoring group without ID: ", actualGroup)
			continue
		}

		actualGroupID := *actualGroup.GroupId

		adding, found := instanceSecurityGroupIds[actualGroupID]
		if found && adding {
			// We don't need to make a change; the permission is already in place
			delete(instanceSecurityGroupIds, actualGroupID)
		} else {
			// This group is not needed by allInstances; delete it
			instanceSecurityGroupIds[actualGroupID] = false
		}
	}

	for instanceSecurityGroupId, add := range instanceSecurityGroupIds {
		if add {
			glog.V(2).Infof("Adding rule for traffic from the load balancer (%s) to instances (%s)", loadBalancerSecurityGroupId, instanceSecurityGroupId)
		} else {
			glog.V(2).Infof("Removing rule for traffic from the load balancer (%s) to instance (%s)", loadBalancerSecurityGroupId, instanceSecurityGroupId)
		}
		sourceGroupId := &ec2.UserIdGroupPair{}
		sourceGroupId.GroupId = &loadBalancerSecurityGroupId

		allProtocols := "-1"

		permission := &ec2.IpPermission{}
		permission.IpProtocol = &allProtocols
		permission.UserIdGroupPairs = []*ec2.UserIdGroupPair{sourceGroupId}

		permissions := []*ec2.IpPermission{permission}

		if add {
			changed, err := s.ensureSecurityGroupIngress(instanceSecurityGroupId, permissions)
			if err != nil {
				return err
			}
			if !changed {
				glog.Warning("Allowing ingress was not needed; concurrent change? groupId=", instanceSecurityGroupId)
			}
		} else {
			changed, err := s.removeSecurityGroupIngress(instanceSecurityGroupId, permissions)
			if err != nil {
				return err
			}
			if !changed {
				glog.Warning("Revoking ingress was not needed; concurrent change? groupId=", instanceSecurityGroupId)
			}
		}
	}

	return nil
}

// EnsureLoadBalancerDeleted implements LoadBalancer.EnsureLoadBalancerDeleted.
func (s *AWSCloud) EnsureLoadBalancerDeleted(name, region string) error {
	if region != s.region {
		return fmt.Errorf("requested load balancer region '%s' does not match cluster region '%s'", region, s.region)
	}

	lb, err := s.describeLoadBalancer(name)
	if err != nil {
		return err
	}

	if lb == nil {
		glog.Info("Load balancer already deleted: ", name)
		return nil
	}

	{
		// De-authorize the load balancer security group from the instances security group
		err = s.updateInstanceSecurityGroupsForLoadBalancer(lb, nil)
		if err != nil {
			glog.Error("Error deregistering load balancer from instance security groups: ", err)
			return err
		}
	}

	{
		// Delete the load balancer itself
		request := &elb.DeleteLoadBalancerInput{}
		request.LoadBalancerName = lb.LoadBalancerName

		_, err = s.elb.DeleteLoadBalancer(request)
		if err != nil {
			// TODO: Check if error was because load balancer was concurrently deleted
			glog.Error("Error deleting load balancer: ", err)
			return err
		}
	}

	{
		// Delete the security group(s) for the load balancer
		// Note that this is annoying: the load balancer disappears from the API immediately, but it is still
		// deleting in the background.  We get a DependencyViolation until the load balancer has deleted itself

		// Collect the security groups to delete
		securityGroupIDs := map[string]struct{}{}
		for _, securityGroupID := range lb.SecurityGroups {
			if isNilOrEmpty(securityGroupID) {
				glog.Warning("Ignoring empty security group in ", name)
				continue
			}
			securityGroupIDs[*securityGroupID] = struct{}{}
		}

		// Loop through and try to delete them
		timeoutAt := time.Now().Add(time.Second * 300)
		for {
			for securityGroupID := range securityGroupIDs {
				request := &ec2.DeleteSecurityGroupInput{}
				request.GroupId = &securityGroupID
				_, err := s.ec2.DeleteSecurityGroup(request)
				if err == nil {
					delete(securityGroupIDs, securityGroupID)
				} else {
					ignore := false
					if awsError, ok := err.(awserr.Error); ok {
						if awsError.Code() == "DependencyViolation" {
							glog.V(2).Infof("Ignoring DependencyViolation while deleting load-balancer security group (%s), assuming because LB is in process of deleting", securityGroupID)
							ignore = true
						}
					}
					if !ignore {
						return fmt.Errorf("error while deleting load balancer security group (%s): %v", securityGroupID, err)
					}
				}
			}

			if len(securityGroupIDs) == 0 {
				glog.V(2).Info("Deleted all security groups for load balancer: ", name)
				break
			}

			if time.Now().After(timeoutAt) {
				return fmt.Errorf("timed out waiting for load-balancer deletion: %s", name)
			}

			glog.V(2).Info("Waiting for load-balancer to delete so we can delete security groups: ", name)

			time.Sleep(5 * time.Second)
		}
	}

	return nil
}

// UpdateLoadBalancer implements LoadBalancer.UpdateLoadBalancer
func (s *AWSCloud) UpdateLoadBalancer(name, region string, hosts []string) error {
	if region != s.region {
		return fmt.Errorf("requested load balancer region '%s' does not match cluster region '%s'", region, s.region)
	}

	instances, err := s.getInstancesByNodeNames(hosts)
	if err != nil {
		return err
	}

	lb, err := s.describeLoadBalancer(name)
	if err != nil {
		return err
	}

	if lb == nil {
		return fmt.Errorf("Load balancer not found")
	}

	err = s.ensureLoadBalancerInstances(orEmpty(lb.LoadBalancerName), lb.Instances, instances)
	if err != nil {
		return nil
	}

	err = s.updateInstanceSecurityGroupsForLoadBalancer(lb, instances)
	if err != nil {
		return err
	}

	return nil
}

// Returns the instance with the specified ID
func (a *AWSCloud) getInstanceByID(instanceID string) (*ec2.Instance, error) {
	instances, err := a.getInstancesByIDs([]*string{&instanceID})
	if err != nil {
		return nil, err
	}

	if len(instances) == 0 {
		return nil, fmt.Errorf("no instances found for instance: %s", instanceID)
	}
	if len(instances) > 1 {
		return nil, fmt.Errorf("multiple instances found for instance: %s", instanceID)
	}

	return instances[instanceID], nil
}

func (a *AWSCloud) getInstancesByIDs(instanceIDs []*string) (map[string]*ec2.Instance, error) {
	instancesByID := make(map[string]*ec2.Instance)
	if len(instanceIDs) == 0 {
		return instancesByID, nil
	}

	request := &ec2.DescribeInstancesInput{
		InstanceIds: instanceIDs,
	}

	instances, err := a.ec2.DescribeInstances(request)
	if err != nil {
		return nil, err
	}

	for _, instance := range instances {
		instanceID := orEmpty(instance.InstanceId)
		if instanceID == "" {
			continue
		}

		instancesByID[instanceID] = instance
	}

	return instancesByID, nil
}

// Fetches instances by node names; returns an error if any cannot be found.
// This is currently implemented by fetching all the instances, because this is currently called for all nodes (i.e. the majority)
// In practice, the breakeven vs looping through and calling getInstanceByNodeName is probably around N=2.
func (a *AWSCloud) getInstancesByNodeNames(nodeNames []string) ([]*ec2.Instance, error) {
	allInstances, err := a.getAllInstances()
	if err != nil {
		return nil, err
	}

	nodeNamesMap := make(map[string]int, len(nodeNames))
	for i, nodeName := range nodeNames {
		nodeNamesMap[nodeName] = i
	}

	instances := make([]*ec2.Instance, len(nodeNames))
	for _, instance := range allInstances {
		nodeName := aws.StringValue(instance.PrivateDnsName)
		if nodeName == "" {
			if isAlive(instance) {
				glog.V(2).Infof("ignoring ec2 instance with no PrivateDnsName: %q", aws.StringValue(instance.InstanceId))
			}
			continue
		}
		i, found := nodeNamesMap[nodeName]
		if !found {
			continue
		}
		instances[i] = instance
	}

	for i, instance := range instances {
		if instance == nil {
			nodeName := nodeNames[i]
			return nil, fmt.Errorf("unable to find instance %q", nodeName)
		}
	}

	return instances, nil
}

// Returns all instances that are tagged as being in this cluster.
func (a *AWSCloud) getAllInstances() ([]*ec2.Instance, error) {
	filters := []*ec2.Filter{}
	filters = a.addFilters(filters)
	request := &ec2.DescribeInstancesInput{
		Filters: filters,
	}

	return a.ec2.DescribeInstances(request)
}

// Returns the instance with the specified node name
// Returns nil if it does not exist
func (a *AWSCloud) findInstanceByNodeName(nodeName string) (*ec2.Instance, error) {
	filters := []*ec2.Filter{
		newEc2Filter("private-dns-name", nodeName),
	}
	filters = a.addFilters(filters)
	request := &ec2.DescribeInstancesInput{
		Filters: filters,
	}

	instances, err := a.ec2.DescribeInstances(request)
	if err != nil {
		return nil, err
	}
	if len(instances) == 0 {
		return nil, nil
	}
	if len(instances) > 1 {
		return nil, fmt.Errorf("multiple instances found for name: %s", nodeName)
	}
	return instances[0], nil
}

// Returns the instance with the specified node name
// Like findInstanceByNodeName, but returns error if node not found
func (a *AWSCloud) getInstanceByNodeName(nodeName string) (*ec2.Instance, error) {
	instance, err := a.findInstanceByNodeName(nodeName)
	if err == nil && instance == nil {
		return nil, fmt.Errorf("no instances found for name: %s", nodeName)
	}
	return instance, err
}

// Add additional filters, to match on our tags
// This lets us run multiple k8s clusters in a single EC2 AZ
func (s *AWSCloud) addFilters(filters []*ec2.Filter) []*ec2.Filter {
	for k, v := range s.filterTags {
		filters = append(filters, newEc2Filter("tag:"+k, v))
	}
	return filters
}

// Returns the cluster name or an empty string
func (s *AWSCloud) getClusterName() string {
	return s.filterTags[TagNameKubernetesCluster]
}
