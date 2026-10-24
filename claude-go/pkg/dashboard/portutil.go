package dashboard

import (
	"fmt"
	"net"
)

// tryListen 在 127.0.0.1 上尝试监听指定端口; 返回的 listener 由调用方负责 Close。
//
// 只绑回环地址: dashboard 不该被外部直接访问 (对外由反向代理/飞书入口承担)。
func tryListen(port int) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
}
