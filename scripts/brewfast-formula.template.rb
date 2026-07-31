# typed: strict
# frozen_string_literal: true

# Rendered by the release workflow; do not hand-edit.
class Brewfast < Formula
  desc "Accelerate Homebrew cask installs from throttled GitHub release assets"
  homepage "https://github.com/amitray007/brewfast"
  version "{{VERSION}}"

  on_macos do
    on_arm do
      url "https://github.com/amitray007/homebrew-tap/releases/download/{{TAG}}/brewfast-{{VERSION}}-darwin-arm64.tar.gz"
      sha256 "{{DARWIN_ARM64_SHA256}}"
    end
    on_intel do
      url "https://github.com/amitray007/homebrew-tap/releases/download/{{TAG}}/brewfast-{{VERSION}}-darwin-x64.tar.gz"
      sha256 "{{DARWIN_X64_SHA256}}"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/amitray007/homebrew-tap/releases/download/{{TAG}}/brewfast-{{VERSION}}-linux-arm64.tar.gz"
      sha256 "{{LINUX_ARM64_SHA256}}"
    end
    on_intel do
      url "https://github.com/amitray007/homebrew-tap/releases/download/{{TAG}}/brewfast-{{VERSION}}-linux-x64.tar.gz"
      sha256 "{{LINUX_X64_SHA256}}"
    end
  end

  def install
    bin.install "brewfast"
  end

  test do
    assert_equal "brewfast #{version}", shell_output("#{bin}/brewfast --version").strip
    assert_match "brewfast", shell_output("#{bin}/brewfast --help")
  end
end
