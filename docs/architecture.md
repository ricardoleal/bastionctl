# Connection architecture

## How it works

- bastionctl resolves AWS credentials with STS and lists saved connections per account.
- For a new connection, it discovers VPCs and running instances, then checks reachability through SSM or direct TCP.
- Candidate Security Group egress is checked against VPC CIDRs; restricted egress ranks lower.
- You choose a candidate, verify it with SSH, choose an SSH user and key if needed, then save it as a VPC or dedicated-network connection.
- Later runs recheck the saved target and print, launch, or background the tunnel.

bastionctl hands off tunneling to your system's `sshuttle` binary. It does not
implement the tunnel or store SSH credentials. Run the printed command as your
normal user; `sshuttle` elevates its firewall helper when needed. bastionctl
uses `sudo` only for opt-in split-DNS resolver setup.

## SSM-brokered connection

An SSM-managed instance does not need a public IP or inbound SSH port. The
operator's SSH configuration starts an AWS Session Manager connection, and
`sshuttle` carries traffic through that SSH connection.

```mermaid
sequenceDiagram
    participant Operator
    participant bastionctl
    participant AWS API
    participant SSM Service
    participant EC2 Instance (private)
    participant VPC Resources

    Note over Operator,EC2 Instance (private): Discovery is read-only
    Operator->>bastionctl: ./bastionctl --start my-vpc
    bastionctl->>AWS API: ssm:DescribeInstanceInformation
    AWS API-->>bastionctl: Instances reachable through SSM
    bastionctl->>AWS API: ec2:DescribeSecurityGroups
    AWS API-->>bastionctl: Egress rules
    bastionctl->>bastionctl: Rank candidates

    Operator->>bastionctl: Select pivot instance
    bastionctl->>bastionctl: Print sshuttle command
    Operator->>SSM Service: aws ssm start-session --target i-abc123
    SSM Service->>EC2 Instance (private): SSH session
    Operator->>EC2 Instance (private): sshuttle -r i-abc123 10.0.0.0/16
    EC2 Instance (private)->>VPC Resources: Forward traffic
```

The instance needs no public IP or inbound Security Group rule. AWS IAM
authorizes the SSM session, and SSH uses the operator's existing
`~/.ssh/config` and agent. bastionctl's AWS discovery calls are read-only;
the actual tunnel is created by `sshuttle` and SSH.

## Direct TCP connection

For an instance reachable over a public network or VPN, bastionctl probes SSH
and ranks reachable candidates. The chosen SSH target is then used by
`sshuttle`.

```mermaid
sequenceDiagram
    participant Operator
    participant bastionctl
    participant EC2 Instance
    participant VPC Resources

    Operator->>bastionctl: ./bastionctl --start my-vpc
    bastionctl->>EC2 Instance: TCP probe on port 22
    EC2 Instance-->>bastionctl: SSH banner
    bastionctl->>bastionctl: Rank candidate
    Operator->>bastionctl: Select pivot instance
    bastionctl->>bastionctl: Print sshuttle command
    Operator->>EC2 Instance: sshuttle -r 52.x.x.x 10.0.0.0/16
    EC2 Instance->>VPC Resources: Forward traffic
```