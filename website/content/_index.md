---
title: ephemerd
layout: hextra-home
---

{{< hextra/hero-badge link="https://github.com/ephpm/ephemerd/releases/latest" >}}
  Latest Release
{{< /hextra/hero-badge >}}

<div class="hx-mt-6 hx-mb-6">
{{< hextra/hero-headline >}}
  Ephemeral GitHub Actions runners.&nbsp;<br class="sm:hx-block hx-hidden" />One binary, every platform.
{{< /hextra/hero-headline >}}
</div>

<div class="hx-pb-16">
{{< hextra/hero-subtitle >}}
  Secure, isolated, disposable CI environments on Linux, Windows, and macOS.&nbsp;<br class="sm:hx-block hx-hidden" />No Kubernetes. No Docker Desktop. Just one binary.
{{< /hextra/hero-subtitle >}}
</div>

<div class="hx-mb-6">
{{< hextra/hero-button text="Get Started" link="getting-started/" >}}
{{< hextra/hero-button text="GitHub" link="https://github.com/ephpm/ephemerd" style="alt" >}}
</div>

<div class="hx-mt-6"></div>

{{< hextra/feature-grid >}}
  {{< hextra/feature-card
    title="Single Binary"
    subtitle="Embeds containerd as a Go library (like k3s). No runtime dependencies beyond the OS kernel."
    style="background: radial-gradient(ellipse at 50% 80%,rgba(45,112,210,0.15),hsla(0,0%,100%,0));"
  >}}
  {{< hextra/feature-card
    title="Every Platform"
    subtitle="Linux containers, Hyper-V on Windows, Virtualization.framework on macOS. Same OCI images everywhere."
    style="background: radial-gradient(ellipse at 50% 80%,rgba(72,180,97,0.15),hsla(0,0%,100%,0));"
  >}}
  {{< hextra/feature-card
    title="Secure by Default"
    subtitle="Every job runs in full isolation. Ephemeral environments destroyed after each run. Network firewall blocks LAN access."
    style="background: radial-gradient(ellipse at 50% 80%,rgba(221,74,57,0.15),hsla(0,0%,100%,0));"
  >}}
  {{< hextra/feature-card
    title="Multi-Forge"
    subtitle="GitHub Actions, Forgejo, Gitea, GitLab, and Woodpecker CI. One daemon, any forge."
  >}}
  {{< hextra/feature-card
    title="Zero Config"
    subtitle="Polling mode works behind NAT with no inbound ports. Or opt into webhook tunnels for instant delivery."
  >}}
  {{< hextra/feature-card
    title="macOS Native"
    subtitle="Per-job macOS VMs via APFS clone-on-write. Xcode, Swift, code signing — all ephemeral."
  >}}
{{< /hextra/feature-grid >}}
