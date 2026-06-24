# Homebrew formula for opencloudcosts — MCP server for cloud pricing.
# This formula is part of the x7even/homebrew-opencloudcosts tap.
#
# SHA256 placeholders below are replaced at release time (see Formula/README.md).

class Opencloudcosts < Formula
  desc "MCP server for cloud pricing (AWS, GCP, Azure)"
  homepage "https://github.com/x7even/cloudcostsmcp"
  version "1.0.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/x7even/cloudcostsmcp/releases/download/v#{version}/opencloudcosts_#{version}_darwin_arm64.tar.gz"
      sha256 "REPLACE_SHA256_DARWIN_ARM64"
    end
    on_intel do
      url "https://github.com/x7even/cloudcostsmcp/releases/download/v#{version}/opencloudcosts_#{version}_darwin_amd64.tar.gz"
      sha256 "REPLACE_SHA256_DARWIN_AMD64"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/x7even/cloudcostsmcp/releases/download/v#{version}/opencloudcosts_#{version}_linux_arm64.tar.gz"
      sha256 "REPLACE_SHA256_LINUX_ARM64"
    end
    on_intel do
      url "https://github.com/x7even/cloudcostsmcp/releases/download/v#{version}/opencloudcosts_#{version}_linux_amd64.tar.gz"
      sha256 "REPLACE_SHA256_LINUX_AMD64"
    end
  end

  def install
    bin.install "opencloudcosts"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/opencloudcosts --version")
  end
end
