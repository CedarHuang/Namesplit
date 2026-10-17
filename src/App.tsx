import { useEffect, useState } from "react";
import { ConfigProvider, Layout, Typography, theme } from "antd";
import zhCN from "antd/locale/zh_CN";

const { Content, Header } = Layout;
const { Paragraph, Title } = Typography;

export default function App() {
  const [dark, setDark] = useState(
    () => window.matchMedia("(prefers-color-scheme: dark)").matches,
  );

  useEffect(() => {
    const query = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = (event: MediaQueryListEvent) => setDark(event.matches);
    query.addEventListener("change", onChange);
    return () => query.removeEventListener("change", onChange);
  }, []);

  return (
    <ConfigProvider
      locale={zhCN}
      theme={{ algorithm: dark ? theme.darkAlgorithm : theme.defaultAlgorithm }}
    >
      <Layout style={{ minHeight: "100vh" }}>
        <Header style={{ background: "transparent" }}>
          <Title level={3} style={{ margin: 0, lineHeight: "64px" }}>
            Namesplit
          </Title>
        </Header>
        <Content style={{ padding: 24 }}>
          <Paragraph>界面开发中。</Paragraph>
        </Content>
      </Layout>
    </ConfigProvider>
  );
}
