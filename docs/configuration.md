# Configuration

Connections are stored per AWS account:

```text
~/.config/bastionctl/accounts/<account-id>/<connection>.yaml
```

`$XDG_CONFIG_HOME` and `--config-dir` can change the configuration directory.
When writing a file manually, the `account_id` must match its parent folder;
restrict file permissions with `chmod 600`.

## Example

```yaml
name: corp
kind: vpc                        # vpc | dedicated
account_id: "123456789012"
region: eu-central-1
vpc_id: vpc-0abc123
vpc_cidrs:
  - 10.0.0.0/16
subnet_id: subnet-0abc123
instance_id: i-0abc123
name_tag: pivot
routes:
  - cidr: 10.0.0.0/16
    source: pivot_vpc
    selected: true
connection_method: ssm            # ssm | direct_ip
target: i-0abc123                # instance ID for ssm, IP for direct_ip
ssh_port: 22
dns: # optional; omit for no DNS handling
  mode: split                     # off | all | split
  resolver:
    - 10.0.0.2
  domains:
    - corp.internal
    - svc.cluster.local
  manage_resolver: false
```

For a direct-IP target, provide SSH credentials as needed:

```yaml
connection_method: direct_ip
target: 52.10.20.30
ssh_user: ec2-user
ssh_key_path: ~/.ssh/corp.pem
```

For SSM targets, leave `ssh_user` and `ssh_key_path` blank to use the existing
`~/.ssh/config` and `ssh-agent`. See [SSM prerequisites](requirements.md#ssm-prerequisites).
DNS fields and platform behavior are covered in the [Split DNS guide](split-dns.md).