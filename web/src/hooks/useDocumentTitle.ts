import { useEffect } from 'react';
import { useLocation } from 'react-router-dom';

const TITLES: Record<string, string> = {
  '/': '数据概览 — 行云API',
  '/accounts': '账号管理 — 行云API',
  '/settings': '系统设置 — 行云API',
};

const DEFAULT_TITLE = '行云API';

const useDocumentTitle = () => {
  const location = useLocation();
  useEffect(() => {
    if (location.pathname.startsWith('/accounts/')) {
      const key = decodeURIComponent(location.pathname.replace('/accounts/', ''));
      document.title = `${key} — 账号详情 — 行云API`;
    } else {
      document.title = TITLES[location.pathname] || DEFAULT_TITLE;
    }
  }, [location.pathname]);
};

export default useDocumentTitle;
