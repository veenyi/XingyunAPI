package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/auth"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/store"
)

var resetPasswordCmd = &cobra.Command{
	Use:   "reset-password",
	Short: "重置 Dashboard root 密码",
	Long:  "重置 Dashboard 管理界面的 root 用户密码。如果忘记密码，可以用这个命令重新设置。\n\n注意：默认重置的是当前用户 HOME 下的数据库（~/.joycode-proxy/proxy.db）。\n服务实际使用的数据库由服务运行用户的 HOME 决定（如 NAS 上为 @apphome/xingyun-api）。\n如不确定，请用 --db 显式指定服务数据库文件路径。",
	GroupID: "core",
	Example: `  # 交互式重置密码
  joycode-proxy reset-password

  # 直接指定新密码
  joycode-proxy reset-password -p my_new_password

  # 指定服务数据库文件（推荐，NAS 上服务数据库在服务用户 HOME 下）
  joycode-proxy reset-password -p my_new_password --db /vol1/@apphome/xingyun-api/.joycode-proxy/proxy.db`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		if dbPath == "" {
			def, err := store.DefaultDBPath()
			if err != nil {
				return fmt.Errorf("获取默认数据库路径失败: %w", err)
			}
			dbPath = def
		}
		absPath, _ := filepath.Abs(dbPath)

		s, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("打开数据库失败 (%s): %w", absPath, err)
		}
		defer s.Close()

		newPw, _ := cmd.Flags().GetString("new-password")
		if newPw == "" {
			fmt.Print("请输入新密码（至少 6 位）: ")
			fmt.Scanln(&newPw)
		}

		if len(newPw) < 6 {
			return fmt.Errorf("密码长度不能少于 6 位")
		}

		hash, err := auth.HashPassword(newPw)
		if err != nil {
			return fmt.Errorf("密码加密失败: %w", err)
		}

		if err := s.SetSetting("auth_password_hash", hash); err != nil {
			return fmt.Errorf("保存密码失败: %w", err)
		}

		if s.GetSetting("auth_jwt_secret") == "" {
			b := make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				return fmt.Errorf("生成 JWT secret 失败: %w", err)
			}
			if err := s.SetSetting("auth_jwt_secret", hex.EncodeToString(b)); err != nil {
				return fmt.Errorf("保存 JWT secret 失败: %w", err)
			}
		}

		fmt.Printf("密码重置成功（数据库: %s）\n", absPath)
		return nil
	},
}

func init() {
	resetPasswordCmd.Flags().StringP("new-password", "p", "", "新密码")
	resetPasswordCmd.Flags().String("db", "", "SQLite 数据库文件路径（默认 ~/.joycode-proxy/proxy.db）")
	rootCmd.AddCommand(resetPasswordCmd)
}
