import React, { useContext, useEffect, useState } from 'react';
import { Layout, Menu, Typography, Tag, theme, Tooltip, Button, message, Dropdown } from 'antd';
import {
  DashboardOutlined,
  AppstoreOutlined,
  TeamOutlined,
  SettingOutlined,
  MessageOutlined,
  CheckCircleOutlined,
  GithubOutlined,
  StarFilled,
  LogoutOutlined,
  SunOutlined,
  MoonOutlined,
  DesktopOutlined,
} from '@ant-design/icons';
import { useNavigate, useLocation, Outlet } from 'react-router-dom';
import useDocumentTitle from '../hooks/useDocumentTitle';
import { ThemeContext } from '../App';
import { api, clearToken } from '../api';

const { Header, Sider, Content } = Layout;
const { Text } = Typography;

const menuItems = [
  { key: '/dashboard', icon: <DashboardOutlined />, label: '数据概览' },
  { key: '/chat', icon: <MessageOutlined />, label: '聊天' },
  { key: '/accounts', icon: <TeamOutlined />, label: '账号管理' },
  { key: '/models', icon: <AppstoreOutlined />, label: '模型与渠道' },
  { key: '/settings', icon: <SettingOutlined />, label: '系统设置' },
];

const COLLAPSED_KEY = 'joycode_sider_collapsed';

const MainLayout: React.FC = () => {
  const [collapsed, setCollapsed] = useState(() => localStorage.getItem(COLLAPSED_KEY) === 'true');
  const navigate = useNavigate();
  const location = useLocation();
  const { token } = theme.useToken();
  useDocumentTitle();
  const { theme: appTheme, setTheme: setAppTheme } = useContext(ThemeContext);

  const themeIcon = appTheme === 'dark' ? <MoonOutlined /> : appTheme === 'light' ? <SunOutlined /> : <DesktopOutlined />;

  const [healthStatus, setHealthStatus] = useState<'ok' | 'error'>('ok');
  const [accountCount, setAccountCount] = useState(0);
  const [stars, setStars] = useState<number | null>(null);

  useEffect(() => {
    api.getHealth().then((h) => {
      setHealthStatus(h.status === 'ok' ? 'ok' : 'error');
      setAccountCount(h.accounts);
    }).catch(() => setHealthStatus('error'));
    api.getGitHubStars().then((s) => { if (s > 0) setStars(s); }).catch(() => {});
  }, []);

  const selectedKey = location.pathname.startsWith('/accounts') ? '/accounts'
    : location.pathname.startsWith('/settings') ? '/settings'
    : location.pathname.startsWith('/chat') ? '/chat'
    : '/dashboard';

  return (
    <Layout style={{ minHeight: '100vh', height: '100vh', overflow: 'hidden' }}>
      <Sider
        collapsible
        collapsed={collapsed}
        onCollapse={(val) => { setCollapsed(val); localStorage.setItem(COLLAPSED_KEY, String(val)); }}
        width={220}
      >
        <div style={{
          height: 56,
          display: 'flex',
          alignItems: 'center',
          justifyContent: collapsed ? 'center' : 'flex-start',
          padding: collapsed ? 0 : '0 20px',
          borderBottom: `1px solid ${token.colorBorderSecondary}`,
          gap: 10,
        }}>
          <img
            src="/logo.png"
            alt="行云API"
            style={{
              width: 28,
              height: 28,
              borderRadius: 6,
              flexShrink: 0,
              objectFit: 'contain',
            }}
          />
          {!collapsed && <Text strong style={{ fontSize: 14, letterSpacing: 0.2 }}>行云API</Text>}
        </div>
        <Menu
          mode="inline"
          selectedKeys={[selectedKey]}
          items={menuItems}
          onClick={({ key }) => navigate(key)}
        />
      </Sider>
      <Layout>
        <Header style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 12, minWidth: 0 }}>
            <Tag color={healthStatus === 'ok' ? 'success' : 'error'} icon={<CheckCircleOutlined />}>
              {healthStatus === 'ok' ? '服务正常' : '服务异常'}
            </Tag>
            <Text type="secondary" style={{ fontSize: 13 }}>{accountCount} 个账号在线</Text>
          </div>
          <div style={{ display: 'flex', alignItems: 'center', gap: 16, flexShrink: 0 }}>
            <Tooltip title="退出登录">
              <Button
                type="text"
                icon={<LogoutOutlined />}
                onClick={() => {
                  clearToken();
                  message.success('已退出登录');
                  window.location.href = '/login';
                }}
              />
            </Tooltip>
            <Dropdown
              menu={{
                selectable: true,
                selectedKeys: [appTheme],
                items: [
                  { key: 'auto', label: '跟随系统', icon: <DesktopOutlined /> },
                  { key: 'light', label: '浅色', icon: <SunOutlined /> },
                  { key: 'dark', label: '深色', icon: <MoonOutlined /> },
                ],
                onClick: ({ key }) => setAppTheme(key),
              }}
            >
              <Button
                type="text"
                icon={themeIcon}
                style={{ color: token.colorTextSecondary, fontSize: 16 }}
              />
            </Dropdown>
            <Tooltip title="去 GitHub Star 支持我们">
            <a
              href="https://github.com/veenyi/XingyunAPI"
              target="_blank"
              rel="noopener noreferrer"
              style={{ display: 'flex', alignItems: 'center', gap: 6, color: token.colorTextSecondary, fontSize: 13, textDecoration: 'none', transition: 'color 200ms ease' }}
            >
              <GithubOutlined style={{ fontSize: 18 }} />
              GitHub
              {stars !== null && (
                <span style={{ display: 'inline-flex', alignItems: 'center', gap: 3, marginLeft: 2 }}>
                  <StarFilled style={{ fontSize: 13, color: '#F59E0B' }} />
                  <span style={{ fontSize: 12 }}>{stars.toLocaleString()}</span>
                </span>
              )}
            </a>
          </Tooltip>
          </div>
        </Header>
        <Content style={{ overflow: 'auto' }}>
          <Outlet />
        </Content>
      </Layout>
    </Layout>
  );
};

export default MainLayout;
