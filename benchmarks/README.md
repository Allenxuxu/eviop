## eviop 压测

压测工具:

-  [tcpkali](https://github.com/machinezone/tcpkali) for Echo

| OS       | Package manager                         | Command                |
| -------- | --------------------------------------- | ---------------------- |
| Mac OS X | [Homebrew](http://brew.sh/)             | `brew install tcpkali` |
| Mac OS X | [MacPorts](https://www.macports.org/)   | `port install tcpkali` |
| FreeBSD  | [pkgng](https://wiki.freebsd.org/pkgng) | `pkg install tcpkali`  |
| Linux    | [nix](https://nixos.org/nix/)           | `nix-env -i tcpkali`   |

运行：

```bash
 ./bench.sh      
```

## 备注

-  当前压测结果是在我的 MacBook Air 上运行得出
-  压测程序运行在单线程模式下 (GOMAXPROC=1)
-  TCP 客户端（Ipv4）在本地（localhost）连接
