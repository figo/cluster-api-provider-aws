/*
Copyright 2018 The Kubernetes Authors.

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

package ec2

import (
	"fmt"

	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/converters"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/filter"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/tags"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/pkg/errors"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/apis/infrastructure/v1alpha2"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/awserrors"
)

const (
	// IPProtocolTCP is how EC2 represents the TCP protocol in ingress rules
	IPProtocolTCP = "tcp"

	// IPProtocolUDP is how EC2 represents the UDP protocol in ingress rules
	IPProtocolUDP = "udp"

	// IPProtocolICMP is how EC2 represents the ICMP protocol in ingress rules
	IPProtocolICMP = "icmp"

	// IPProtocolICMPv6 is how EC2 represents the ICMPv6 protocol in ingress rules
	IPProtocolICMPv6 = "58"
)

func (s *Service) reconcileSecurityGroups() error {
	s.scope.V(2).Info("Reconciling security groups")

	if s.scope.Network().SecurityGroups == nil {
		s.scope.Network().SecurityGroups = make(map[v1alpha2.SecurityGroupRole]v1alpha2.SecurityGroup)
	}

	sgs, err := s.describeSecurityGroupsByName()
	if err != nil {
		return err
	}

	// Declare all security group roles that the reconcile loop takes care of.
	roles := []v1alpha2.SecurityGroupRole{
		v1alpha2.SecurityGroupBastion,
		v1alpha2.SecurityGroupControlPlane,
		v1alpha2.SecurityGroupNode,
		v1alpha2.SecurityGroupLB,
	}

	// First iteration makes sure that the security group are valid and fully created.
	for _, role := range roles {
		sg := s.getDefaultSecurityGroup(role)
		existing, ok := sgs[*sg.GroupName]

		if !ok {
			if err := s.createSecurityGroup(role, sg); err != nil {
				return err
			}

			s.scope.SecurityGroups()[role] = v1alpha2.SecurityGroup{
				ID:   *sg.GroupId,
				Name: *sg.GroupName,
			}
			s.scope.V(2).Info("Created security group for role", "role", role, "security-group", s.scope.SecurityGroups()[role])
			continue
		}

		// TODO(vincepri): validate / update security group if necessary.
		s.scope.SecurityGroups()[role] = existing

		// Make sure tags are up to date.
		err := tags.Ensure(existing.Tags, &tags.ApplyParams{
			EC2Client:   s.scope.EC2,
			BuildParams: s.getSecurityGroupTagParams(existing.Name, existing.ID, role),
		})

		if err != nil {
			return errors.Wrapf(err, "failed to ensure tags on security group %q", existing.ID)
		}
	}

	// Second iteration creates or updates all permissions on the security group to match
	// the specified ingress rules.
	for role, sg := range s.scope.SecurityGroups() {
		if sg.Tags.HasAWSCloudProviderOwned(s.scope.Name()) {
			// skip rule reconciliation, as we expect the in-cluster cloud integration to manage them
			continue
		}
		current := sg.IngressRules

		want, err := s.getSecurityGroupIngressRules(role)
		if err != nil {
			return err
		}

		toRevoke := current.Difference(want)
		if len(toRevoke) > 0 {
			if err := s.revokeSecurityGroupIngressRules(sg.ID, toRevoke); err != nil {
				return errors.Wrapf(err, "failed to revoke security group ingress rules for %q", sg.ID)
			}

			s.scope.V(2).Info("Revoked ingress rules from security group", "revoked-ingress-rules", toRevoke, "security-group-id", sg.ID)
		}

		toAuthorize := want.Difference(current)
		if len(toAuthorize) > 0 {
			if err := s.authorizeSecurityGroupIngressRules(sg.ID, toAuthorize); err != nil {
				return err
			}

			s.scope.V(2).Info("Authorized ingress rules in security group", "authorized-ingress-rules", toAuthorize, "security-group-id", sg.ID)
		}
	}

	return nil
}

func (s *Service) deleteSecurityGroups() error {
	for _, sg := range s.scope.SecurityGroups() {
		current := sg.IngressRules

		if err := s.revokeAllSecurityGroupIngressRules(sg.ID); awserrors.IsIgnorableSecurityGroupError(err) != nil {
			return err
		}

		s.scope.V(2).Info("Revoked ingress rules from security group", "revoked-ingress-rules", current, "security-group-id", sg.ID)
	}

	for _, sg := range s.scope.SecurityGroups() {
		input := &ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(sg.ID),
		}

		if _, err := s.scope.EC2.DeleteSecurityGroup(input); awserrors.IsIgnorableSecurityGroupError(err) != nil {
			return errors.Wrapf(err, "failed to delete security group %q", sg.ID)
		}

		s.scope.V(2).Info("Deleted security group security group", "security-group-id", sg.ID)
	}

	return nil
}

func (s *Service) describeSecurityGroupsByName() (map[string]v1alpha2.SecurityGroup, error) {
	input := &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			filter.EC2.VPC(s.scope.VPC().ID),
			filter.EC2.Cluster(s.scope.Name()),
		},
	}

	out, err := s.scope.EC2.DescribeSecurityGroups(input)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to describe security groups in vpc %q", s.scope.VPC().ID)
	}

	res := make(map[string]v1alpha2.SecurityGroup, len(out.SecurityGroups))
	for _, ec2sg := range out.SecurityGroups {
		sg := v1alpha2.SecurityGroup{
			ID:   *ec2sg.GroupId,
			Name: *ec2sg.GroupName,
			Tags: converters.TagsToMap(ec2sg.Tags),
		}

		for _, ec2rule := range ec2sg.IpPermissions {
			sg.IngressRules = append(sg.IngressRules, ingressRuleFromSDKType(ec2rule))
		}

		res[sg.Name] = sg
	}

	return res, nil
}

func (s *Service) createSecurityGroup(role v1alpha2.SecurityGroupRole, input *ec2.SecurityGroup) error {
	out, err := s.scope.EC2.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		VpcId:       input.VpcId,
		GroupName:   input.GroupName,
		Description: aws.String(fmt.Sprintf("Kubernetes cluster %s: %s", s.scope.Name(), role)),
	})

	if err != nil {
		return errors.Wrapf(err, "failed to create security group %q in vpc %q", *input.GroupName, *input.VpcId)
	}

	// Set the group id.
	input.GroupId = out.GroupId

	// Tag the security group.
	if _, err := s.scope.EC2.CreateTags(&ec2.CreateTagsInput{Resources: []*string{out.GroupId}, Tags: input.Tags}); err != nil {
		return errors.Wrapf(err, "failed to tag security group %q in vpc %q", *input.GroupName, *input.VpcId)
	}

	return nil
}

func (s *Service) authorizeSecurityGroupIngressRules(id string, rules v1alpha2.IngressRules) error {
	input := &ec2.AuthorizeSecurityGroupIngressInput{GroupId: aws.String(id)}
	for _, rule := range rules {
		input.IpPermissions = append(input.IpPermissions, ingressRuleToSDKType(rule))
	}

	if _, err := s.scope.EC2.AuthorizeSecurityGroupIngress(input); err != nil {
		return errors.Wrapf(err, "failed to authorize security group %q ingress rules: %v", id, rules)
	}

	return nil
}

func (s *Service) revokeSecurityGroupIngressRules(id string, rules v1alpha2.IngressRules) error {
	input := &ec2.RevokeSecurityGroupIngressInput{GroupId: aws.String(id)}
	for _, rule := range rules {
		input.IpPermissions = append(input.IpPermissions, ingressRuleToSDKType(rule))
	}

	if _, err := s.scope.EC2.RevokeSecurityGroupIngress(input); err != nil {
		return errors.Wrapf(err, "failed to revoke security group %q ingress rules: %v", id, rules)
	}

	return nil
}

func (s *Service) revokeAllSecurityGroupIngressRules(id string) error {
	describeInput := &ec2.DescribeSecurityGroupsInput{GroupIds: []*string{aws.String(id)}}

	securityGroups, err := s.scope.EC2.DescribeSecurityGroups(describeInput)
	if err != nil {
		return errors.Wrapf(err, "failed to query security group %q", id)
	}

	for _, sg := range securityGroups.SecurityGroups {
		if len(sg.IpPermissions) > 0 {
			revokeInput := &ec2.RevokeSecurityGroupIngressInput{
				GroupId:       aws.String(id),
				IpPermissions: sg.IpPermissions,
			}
			if _, err := s.scope.EC2.RevokeSecurityGroupIngress(revokeInput); err != nil {
				return errors.Wrapf(err, "failed to revoke security group %q ingress rules", id)
			}
		}
	}

	return nil
}

func (s *Service) defaultSSHIngressRule(sourceSecurityGroupID string) *v1alpha2.IngressRule {
	return &v1alpha2.IngressRule{
		Description:            "SSH",
		Protocol:               v1alpha2.SecurityGroupProtocolTCP,
		FromPort:               22,
		ToPort:                 22,
		SourceSecurityGroupIDs: []string{sourceSecurityGroupID},
	}
}

func (s *Service) getSecurityGroupIngressRules(role v1alpha2.SecurityGroupRole) (v1alpha2.IngressRules, error) {
	switch role {
	case v1alpha2.SecurityGroupBastion:
		return v1alpha2.IngressRules{
			{
				Description: "SSH",
				Protocol:    v1alpha2.SecurityGroupProtocolTCP,
				FromPort:    22,
				ToPort:      22,
				CidrBlocks:  []string{anyIPv4CidrBlock},
			},
		}, nil
	case v1alpha2.SecurityGroupControlPlane:
		return v1alpha2.IngressRules{
			s.defaultSSHIngressRule(s.scope.SecurityGroups()[v1alpha2.SecurityGroupBastion].ID),
			{
				Description: "Kubernetes API",
				Protocol:    v1alpha2.SecurityGroupProtocolTCP,
				FromPort:    6443,
				ToPort:      6443,
				CidrBlocks:  []string{anyIPv4CidrBlock},
			},
			{
				Description:            "etcd",
				Protocol:               v1alpha2.SecurityGroupProtocolTCP,
				FromPort:               2379,
				ToPort:                 2379,
				SourceSecurityGroupIDs: []string{s.scope.SecurityGroups()[v1alpha2.SecurityGroupControlPlane].ID},
			},
			{
				Description:            "etcd peer",
				Protocol:               v1alpha2.SecurityGroupProtocolTCP,
				FromPort:               2380,
				ToPort:                 2380,
				SourceSecurityGroupIDs: []string{s.scope.SecurityGroups()[v1alpha2.SecurityGroupControlPlane].ID},
			},
			{
				Description: "bgp (calico)",
				Protocol:    v1alpha2.SecurityGroupProtocolTCP,
				FromPort:    179,
				ToPort:      179,
				SourceSecurityGroupIDs: []string{
					s.scope.SecurityGroups()[v1alpha2.SecurityGroupControlPlane].ID,
					s.scope.SecurityGroups()[v1alpha2.SecurityGroupNode].ID,
				},
			},
			{
				Description: "IP-in-IP (calico)",
				Protocol:    v1alpha2.SecurityGroupProtocolIPinIP,
				FromPort:    -1,
				ToPort:      65535,
				SourceSecurityGroupIDs: []string{
					s.scope.SecurityGroups()[v1alpha2.SecurityGroupControlPlane].ID,
					s.scope.SecurityGroups()[v1alpha2.SecurityGroupNode].ID,
				},
			},
		}, nil

	case v1alpha2.SecurityGroupNode:
		return v1alpha2.IngressRules{
			s.defaultSSHIngressRule(s.scope.SecurityGroups()[v1alpha2.SecurityGroupBastion].ID),
			{
				Description: "Node Port Services",
				Protocol:    v1alpha2.SecurityGroupProtocolTCP,
				FromPort:    30000,
				ToPort:      32767,
				CidrBlocks:  []string{anyIPv4CidrBlock},
			},
			{
				Description: "Kubelet API",
				Protocol:    v1alpha2.SecurityGroupProtocolTCP,
				FromPort:    10250,
				ToPort:      10250,
				SourceSecurityGroupIDs: []string{
					s.scope.SecurityGroups()[v1alpha2.SecurityGroupControlPlane].ID,
					// This is needed to support metrics-server deployments
					s.scope.SecurityGroups()[v1alpha2.SecurityGroupNode].ID,
				},
			},
			{
				Description: "bgp (calico)",
				Protocol:    v1alpha2.SecurityGroupProtocolTCP,
				FromPort:    179,
				ToPort:      179,
				SourceSecurityGroupIDs: []string{
					s.scope.SecurityGroups()[v1alpha2.SecurityGroupControlPlane].ID,
					s.scope.SecurityGroups()[v1alpha2.SecurityGroupNode].ID,
				},
			},
			{
				Description: "IP-in-IP (calico)",
				Protocol:    v1alpha2.SecurityGroupProtocolIPinIP,
				FromPort:    -1,
				ToPort:      65535,
				SourceSecurityGroupIDs: []string{
					s.scope.SecurityGroups()[v1alpha2.SecurityGroupNode].ID,
					s.scope.SecurityGroups()[v1alpha2.SecurityGroupControlPlane].ID,
				},
			},
		}, nil
	case v1alpha2.SecurityGroupLB:
		// We hand this group off to the in-cluster cloud provider, so these rules aren't used
		return v1alpha2.IngressRules{}, nil
	}

	return nil, errors.Errorf("Cannot determine ingress rules for unknown security group role %q", role)
}

func (s *Service) getSecurityGroupName(clusterName string, role v1alpha2.SecurityGroupRole) string {
	return fmt.Sprintf("%s-%v", clusterName, role)
}

func (s *Service) getDefaultSecurityGroup(role v1alpha2.SecurityGroupRole) *ec2.SecurityGroup {
	name := s.getSecurityGroupName(s.scope.Name(), role)

	return &ec2.SecurityGroup{
		GroupName: aws.String(name),
		VpcId:     aws.String(s.scope.VPC().ID),
		Tags:      converters.MapToTags(v1alpha2.Build(s.getSecurityGroupTagParams(name, "", role))),
	}
}

func (s *Service) getSecurityGroupTagParams(name string, id string, role v1alpha2.SecurityGroupRole) v1alpha2.BuildParams {
	additional := v1alpha2.Tags{}
	if role == v1alpha2.SecurityGroupLB {
		additional[v1alpha2.ClusterAWSCloudProviderTagKey(s.scope.Name())] = string(v1alpha2.ResourceLifecycleOwned)
	}
	return v1alpha2.BuildParams{
		ClusterName: s.scope.Name(),
		Lifecycle:   v1alpha2.ResourceLifecycleOwned,
		Name:        aws.String(name),
		ResourceID:  id,
		Role:        aws.String(string(role)),
		Additional:  additional,
	}
}

func ingressRuleToSDKType(i *v1alpha2.IngressRule) (res *ec2.IpPermission) {
	// AWS seems to ignore the From/To port when set on protocols where it doesn't apply, but
	// we avoid serializing it out for clarity's sake.
	// See: https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_IpPermission.html
	switch i.Protocol {
	case v1alpha2.SecurityGroupProtocolTCP,
		v1alpha2.SecurityGroupProtocolUDP,
		v1alpha2.SecurityGroupProtocolICMP,
		v1alpha2.SecurityGroupProtocolICMPv6:
		res = &ec2.IpPermission{
			IpProtocol: aws.String(string(i.Protocol)),
			FromPort:   aws.Int64(i.FromPort),
			ToPort:     aws.Int64(i.ToPort),
		}
	default:
		res = &ec2.IpPermission{
			IpProtocol: aws.String(string(i.Protocol)),
		}
	}

	for _, cidr := range i.CidrBlocks {
		ipRange := &ec2.IpRange{
			CidrIp: aws.String(cidr),
		}

		if i.Description != "" {
			ipRange.Description = aws.String(i.Description)
		}

		res.IpRanges = append(res.IpRanges, ipRange)
	}

	for _, groupID := range i.SourceSecurityGroupIDs {
		userIDGroupPair := &ec2.UserIdGroupPair{
			GroupId: aws.String(groupID),
		}

		if i.Description != "" {
			userIDGroupPair.Description = aws.String(i.Description)
		}

		res.UserIdGroupPairs = append(res.UserIdGroupPairs, userIDGroupPair)
	}

	return res
}

func ingressRuleFromSDKType(v *ec2.IpPermission) (res *v1alpha2.IngressRule) {
	// Ports are only well-defined for TCP and UDP protocols, but EC2 overloads the port range
	// in the case of ICMP(v6) traffic to indicate which codes are allowed. For all other protocols,
	// including the custom "-1" All Traffic protcol, FromPort and ToPort are omitted from the response.
	// See: https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_IpPermission.html
	switch *v.IpProtocol {
	case IPProtocolTCP,
		IPProtocolUDP,
		IPProtocolICMP,
		IPProtocolICMPv6:
		res = &v1alpha2.IngressRule{
			Protocol: v1alpha2.SecurityGroupProtocol(*v.IpProtocol),
			FromPort: *v.FromPort,
			ToPort:   *v.ToPort,
		}
	default:
		res = &v1alpha2.IngressRule{
			Protocol: v1alpha2.SecurityGroupProtocol(*v.IpProtocol),
		}
	}

	for _, ec2range := range v.IpRanges {
		if ec2range.Description != nil && *ec2range.Description != "" {
			res.Description = *ec2range.Description
		}

		res.CidrBlocks = append(res.CidrBlocks, *ec2range.CidrIp)
	}

	for _, pair := range v.UserIdGroupPairs {
		if pair.GroupId == nil {
			continue
		}

		if pair.Description != nil && *pair.Description != "" {
			res.Description = *pair.Description
		}

		res.SourceSecurityGroupIDs = append(res.SourceSecurityGroupIDs, *pair.GroupId)
	}

	return res
}
