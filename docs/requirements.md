# IAM permissions and limitations

## Required dependency: sshuttle

bastionctl delegates tunnel creation to the [sshuttle](https://github.com/sshuttle/sshuttle) executable; it does not
implement it. The Homebrew formula installs `sshuttle` as a dependency, so
`brew install bastionctl` includes it automatically. For other installation
methods, install `sshuttle` separately and ensure it is on your `PATH`. Verify
the executable with `sshuttle --version`.

## IAM permissions

bastionctl discovery uses these AWS actions:

```text
ec2:DescribeInstances
ec2:DescribeRegions
ec2:DescribeVpcs
ec2:DescribeSecurityGroups
ec2:DescribeRouteTables
ssm:DescribeInstanceInformation
sts:GetCallerIdentity
```

`ec2:DescribeRouteTables` is used for dedicated-network discovery and is
non-fatal if unavailable. `ec2:DescribeRegions` enables the region picker;
without it, bastionctl falls back to the profile region. `iam:ListAccountAliases`
is optional and displays an account alias in the browser. SSM discovery uses
`ssm:DescribeInstanceInformation`; this is separate from permission to start
an SSM session.

## SSM prerequisites

For private instances, install the Session Manager plugin for the AWS CLI:

```bash
brew install --cask session-manager-plugin
```

Add a proxy entry to `~/.ssh/config`:

```sshconfig
Host i-* mi-*
    ProxyCommand sh -c "aws ssm start-session --target %h --document-name AWS-StartSSHSession --parameters 'portNumber=%p'"
```

bastionctl uses the existing SSH configuration; it does not invoke
`aws ssm start-session` itself. The profile needs permission for the session
that the operator starts.

## Known limitations

- AWS SDK defaults control caching and retry behavior.
- Security Group references to other groups and managed prefix lists are not resolved in the egress check.
- Bulk ranking does not start real SSH or SSM sessions; the real test runs on the final selection.
- Dedicated-network discovery reads only the selected instance's subnet route table; add external ranges manually.
- Split DNS limitations are documented in the [Split DNS guide](split-dns.md#platform-notes-and-limitations).