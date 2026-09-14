# Security Policy

## Reporting a vulnerability

Please **do not open a public issue** for a security problem.

Report it privately through GitHub instead: open the
[Security tab](https://github.com/bughatti/codecsmith/security) of this repository and
choose **Report a vulnerability**. Only the maintainer can see the report.

Include what you found, the version or commit you tested, and the steps to
reproduce it. You should get a first response within a week.

## Supported versions

Only the latest release receives security fixes. If you run an older version,
upgrade first and check whether the problem is still there.

## Scope

In scope: the code in this repository and the container images published from
it at `ghcr.io/bughatti/codecsmith`.

Out of scope: vulnerabilities in third-party software the images include, such
as FFmpeg or the operating system base image, unless this project's
configuration makes them exploitable. Report those upstream.
