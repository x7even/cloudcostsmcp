# Homebrew formula for opencloudcosts — MCP server for cloud pricing.
# This formula is part of the x7even/homebrew-opencloudcosts tap.

class Opencloudcosts < Formula
  desc "MCP server for cloud pricing (AWS, GCP, Azure)"
  homepage "https://github.com/x7even/cloudcostsmcp"
  version "1.1.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/x7even/cloudcostsmcp/releases/download/v#{version}/opencloudcosts_#{version}_darwin_arm64.tar.gz"
      sha256 "1d5eaa3da97e9de420429c95553922d3a194c44e922ae3a347a70617c901eb07"
    end
    on_intel do
      url "https://github.com/x7even/cloudcostsmcp/releases/download/v#{version}/opencloudcosts_#{version}_darwin_amd64.tar.gz"
      sha256 "af5cc564b2e2d9fa7712eaf9a03b89f6576310aaec1842903fba98be0cabd30d"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/x7even/cloudcostsmcp/releases/download/v#{version}/opencloudcosts_#{version}_linux_arm64.tar.gz"
      sha256 "daf0c11fbcadd57deda027f001c235b42975c035c44b8b3790c2db756dabf528"
    end
    on_intel do
      url "https://github.com/x7even/cloudcostsmcp/releases/download/v#{version}/opencloudcosts_#{version}_linux_amd64.tar.gz"
      sha256 "4dd2a4e2b161adcc285def26c151676f388ff7e4208b06db0fb2366e5394e450"
    end
  end

  def install
    bin.install "opencloudcosts"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/opencloudcosts --version")
  end
end
