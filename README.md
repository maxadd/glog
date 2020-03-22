在 glog 的基础之上做了一些修改：

- 日志只输出到一个文件，且文件可以指定；
- 可以指定日志级别，低于该级别的日志并不会输出；
- 可以指定日志文件大小，超过之后会将老日志文件重命名；
- 日志格式有些小修改，增加了年，移除了 pid；
- Fatal 级别只是 Error + 程序退出，并不会输出堆栈信息；
- 使用 `sync.Pool` 存储 buffer。

安装：

```
go get -u github.com/maxadd/glog
```

使用：

```go
package main

import "github.com/maxadd/glog"

func main() {
    logger := glog.NewLogger("/tmp/test.log", "1G", glog.DebugLog, 30)
    defer logger.Flush()
    logger.Debugf("this is %d number", 11)
}
```